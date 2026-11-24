// Package project holds what istari knows about projects on disk (spec
// sections 3 and 4.4): which project a directory belongs to and how two of
// its repositories are linked.
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Link is how two repositories of a project are linked (spec section 4.4).
type Link string

// Links.
const (
	LinkPackage   Link = "package"   // a dependency with a version in a manifest
	LinkSubmodule Link = "submodule" // an entry in .gitmodules
	LinkPath      Link = "path"      // a reference to a neighbouring directory
	LinkNone      Link = "none"      // not determined
)

// Proposal is what repo add proposes for a repository; a human confirms it.
type Proposal struct {
	Link     Link
	DoneWhen string // merged or released
	Evidence string // where the link was found, for the human
}

// SoftAvailable tells whether soft dependencies can be used: not while the
// link is undetermined (spec section 4.4).
func (p Proposal) SoftAvailable() bool { return p.Link != LinkNone }

// DetectLink looks at the manifests of two checked-out repositories of one
// project, in both directions, and proposes how the added one is completed.
// repo is the added repository's owner/repo.
func DetectLink(primaryDir, addedDir, repo string) Proposal {
	for _, try := range []struct{ from, to string }{{primaryDir, addedDir}, {addedDir, primaryDir}} {
		if p, ok := linkFrom(try.from, try.to, repo); ok {
			return p
		}
	}
	return Proposal{Link: LinkNone, DoneWhen: "merged"}
}

// linkFrom finds a reference in the manifests of dir to the repository in
// target. Local paths and submodules win over versions: they are what the
// build actually uses.
func linkFrom(dir, target, repo string) (Proposal, bool) {
	names := identities(target)
	var found []Proposal
	found = append(found, goLinks(dir, names)...)
	found = append(found, npmLinks(dir, names)...)
	found = append(found, pythonLinks(dir, names)...)
	if p, ok := submoduleLink(dir, repo); ok {
		found = append(found, p)
	}
	for _, want := range []Link{LinkPath, LinkSubmodule, LinkPackage} {
		for _, p := range found {
			if p.Link == want {
				return p, true
			}
		}
	}
	return Proposal{}, false
}

func proposal(link Link, evidence string) Proposal {
	done := "merged"
	if link == LinkPackage {
		done = "released"
	}
	return Proposal{Link: link, DoneWhen: done, Evidence: evidence}
}

// identity is how other manifests name a repository's package.
type identity struct {
	goModule, npmName, pyName string
}

func identities(dir string) identity {
	var id identity
	if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		if m := regexp.MustCompile(`(?m)^module\s+(\S+)`).FindSubmatch(data); m != nil {
			id.goModule = string(m[1])
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct{ Name string }
		if json.Unmarshal(data, &pkg) == nil {
			id.npmName = pkg.Name
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml")); err == nil {
		var py struct {
			Project struct{ Name string }
			Tool    struct {
				Poetry struct{ Name string }
			}
		}
		if toml.Unmarshal(data, &py) == nil {
			id.pyName = normalizePy(py.Project.Name + py.Tool.Poetry.Name)
		}
	}
	return id
}

func isLocal(ref string) bool {
	return strings.HasPrefix(ref, ".") || strings.HasPrefix(ref, "/")
}

func goLinks(dir string, id identity) []Proposal {
	if id.goModule == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil
	}
	var out []Proposal
	mod := regexp.QuoteMeta(id.goModule)
	if m := regexp.MustCompile(`(?m)^\s*(?:replace\s+)?` + mod + `(?:\s+\S+)?\s+=>\s+(\S+)`).FindSubmatch(data); m != nil && isLocal(string(m[1])) {
		out = append(out, proposal(LinkPath, fmt.Sprintf("go.mod replaces %s with %s", id.goModule, m[1])))
	}
	if m := regexp.MustCompile(`(?m)^\s*(?:require\s+)?` + mod + `\s+(v\S+)`).FindSubmatch(data); m != nil {
		out = append(out, proposal(LinkPackage, fmt.Sprintf("go.mod requires %s %s", id.goModule, m[1])))
	}
	return out
}

func npmLinks(dir string, id identity) []Proposal {
	if id.npmName == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg map[string]json.RawMessage
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	var out []Proposal
	for _, section := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		var deps map[string]string
		if json.Unmarshal(pkg[section], &deps) != nil {
			continue
		}
		spec, ok := deps[id.npmName]
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(spec, "file:"), strings.HasPrefix(spec, "link:"), strings.HasPrefix(spec, "workspace:"), isLocal(spec):
			out = append(out, proposal(LinkPath, fmt.Sprintf("package.json %s %s: %s", section, id.npmName, spec)))
		default:
			out = append(out, proposal(LinkPackage, fmt.Sprintf("package.json %s %s: %s", section, id.npmName, spec)))
		}
	}
	return out
}

var pyNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+`)

func normalizePy(name string) string {
	return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "-"))
}

func pythonLinks(dir string, id identity) []Proposal {
	if id.pyName == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil {
		return nil
	}
	var py struct {
		Project struct {
			Dependencies []string
		}
		Tool struct {
			Poetry struct {
				Dependencies map[string]any
			}
		}
	}
	if toml.Unmarshal(data, &py) != nil {
		return nil
	}
	var out []Proposal
	for _, dep := range py.Project.Dependencies {
		name := pyNameRe.FindString(dep)
		if normalizePy(name) != id.pyName {
			continue
		}
		if strings.Contains(dep, "@ file:") {
			out = append(out, proposal(LinkPath, "pyproject.toml dependency "+dep))
		} else {
			out = append(out, proposal(LinkPackage, "pyproject.toml dependency "+dep))
		}
	}
	for name, spec := range py.Tool.Poetry.Dependencies {
		if normalizePy(name) != id.pyName {
			continue
		}
		if table, ok := spec.(map[string]any); ok && table["path"] != nil {
			out = append(out, proposal(LinkPath, fmt.Sprintf("pyproject.toml poetry %s path = %v", name, table["path"])))
		} else {
			out = append(out, proposal(LinkPackage, fmt.Sprintf("pyproject.toml poetry %s = %v", name, spec)))
		}
	}
	return out
}

func submoduleLink(dir, repo string) (Proposal, bool) {
	data, err := os.ReadFile(filepath.Join(dir, ".gitmodules"))
	if err != nil {
		return Proposal{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		if r, err := ParseRemote(strings.TrimSpace(value)); err == nil && strings.EqualFold(r, repo) {
			return proposal(LinkSubmodule, ".gitmodules has "+strings.TrimSpace(value)), true
		}
	}
	return Proposal{}, false
}

var remoteRe = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?[^:/]+[:/]([A-Za-z0-9-]+/[A-Za-z0-9._-]+?)(?:\.git)?/?$`)

// ParseRemote returns the owner/repo of a git remote URL.
func ParseRemote(url string) (string, error) {
	m := remoteRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil || strings.HasPrefix(url, "/") || strings.HasPrefix(url, ".") {
		return "", fmt.Errorf("%q is not a remote repository URL", url)
	}
	return m[1], nil
}
