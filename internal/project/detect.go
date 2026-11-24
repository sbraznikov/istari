// Package project holds what istari knows about projects on disk (spec
// sections 3 and 4.4): which project a directory belongs to and how two of
// its repositories are linked.
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
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
// project and proposes how the added one is completed (spec section 4.4).
// When the primary consumes the added repository as a published version, the
// added one is done when released. When it is the other way round, the link
// is recorded but the added repository is done when merged: the primary is
// the one that publishes.
func DetectLink(primaryDir, addedDir, primaryRepo, addedRepo string) Proposal {
	if p, ok := linkFrom(primaryDir, addedDir, addedRepo); ok {
		return p
	}
	if p, ok := linkFrom(addedDir, primaryDir, primaryRepo); ok {
		p.DoneWhen = "merged"
		return p
	}
	return Proposal{Link: LinkNone, DoneWhen: "merged"}
}

// linkFrom finds a reference in the manifests of dir to the repository in
// target, whose owner/repo is repo. Local paths and submodules win over
// versions: they are what the build actually uses. A reference without a
// version (a git URL) is not a link of the table: it proposes none.
func linkFrom(dir, target, repo string) (Proposal, bool) {
	names := identities(target)
	var found []Proposal
	found = append(found, goLinks(dir, names)...)
	found = append(found, npmLinks(dir, names)...)
	found = append(found, pythonLinks(dir, names)...)
	if p, ok := submoduleLink(dir, repo); ok {
		found = append(found, p)
	}
	for _, want := range []Link{LinkPath, LinkSubmodule, LinkPackage, LinkNone} {
		for _, p := range found {
			if p.Link == want {
				return p, true
			}
		}
	}
	return Proposal{}, false
}

var (
	moduleRe  = regexp.MustCompile(`(?m)^module\s+(\S+)`)
	pyNameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+`)
	pySepRe   = regexp.MustCompile(`[-_.]+`)
	npmGitRe  = regexp.MustCompile(`^(git\+|git:|github:|gitlab:|bitbucket:|https?:|[\w.-]+/[\w.-]+(#.*)?$)`)
	remoteRe  = regexp.MustCompile(`^(?:[a-z][a-z0-9+.-]*://(?:[^@/]+@)?[^/]+/|[^@/\s]+@[^:/\s]+:)([A-Za-z0-9-]+/[A-Za-z0-9._-]+?)(?:\.git)?/?$`)
	relRemote = regexp.MustCompile(`^\.\.?/(?:\.\./)*([A-Za-z0-9._-]+?)(?:\.git)?/?$`)
)

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
		if m := moduleRe.FindSubmatch(data); m != nil {
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
			name := py.Project.Name
			if name == "" {
				name = py.Tool.Poetry.Name
			}
			id.pyName = normalizePy(name)
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
		where := fmt.Sprintf("package.json %s %s: %s", section, id.npmName, spec)
		switch {
		case strings.HasPrefix(spec, "file:"), strings.HasPrefix(spec, "link:"), strings.HasPrefix(spec, "workspace:"), isLocal(spec):
			out = append(out, proposal(LinkPath, where))
		case npmGitRe.MatchString(spec):
			out = append(out, proposal(LinkNone, where+" has no version"))
		default:
			out = append(out, proposal(LinkPackage, where))
		}
	}
	return out
}

func normalizePy(name string) string {
	return strings.ToLower(pySepRe.ReplaceAllString(name, "-"))
}

type pyproject struct {
	Project struct {
		Dependencies         []string
		OptionalDependencies map[string][]string `toml:"optional-dependencies"`
	}
	DependencyGroups map[string][]any `toml:"dependency-groups"`
	Tool             struct {
		Poetry struct {
			Dependencies map[string]any
			Group        map[string]struct{ Dependencies map[string]any }
		}
		UV struct {
			Sources map[string]any
		} `toml:"uv"`
	}
}

func pythonLinks(dir string, id identity) []Proposal {
	if id.pyName == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil {
		return nil
	}
	var py pyproject
	if toml.Unmarshal(data, &py) != nil {
		return nil
	}
	var out []Proposal
	// A uv source decides where a dependency comes from.
	for name, src := range py.Tool.UV.Sources {
		table, ok := src.(map[string]any)
		if !ok || normalizePy(name) != id.pyName {
			continue
		}
		switch {
		case table["path"] != nil:
			out = append(out, proposal(LinkPath, fmt.Sprintf("pyproject.toml tool.uv.sources %s path = %v", name, table["path"])))
		case table["git"] != nil:
			out = append(out, proposal(LinkNone, fmt.Sprintf("pyproject.toml tool.uv.sources %s git = %v has no version", name, table["git"])))
		}
	}
	deps := slices.Clone(py.Project.Dependencies)
	for _, group := range py.Project.OptionalDependencies {
		deps = append(deps, group...)
	}
	for _, group := range py.DependencyGroups {
		for _, d := range group {
			if s, ok := d.(string); ok {
				deps = append(deps, s)
			}
		}
	}
	for _, dep := range deps {
		if normalizePy(pyNameRe.FindString(dep)) != id.pyName {
			continue
		}
		switch {
		case strings.Contains(dep, "@ file:"):
			out = append(out, proposal(LinkPath, "pyproject.toml dependency "+dep))
		case strings.Contains(dep, "@"):
			out = append(out, proposal(LinkNone, "pyproject.toml dependency "+dep+" has no version"))
		default:
			out = append(out, proposal(LinkPackage, "pyproject.toml dependency "+dep))
		}
	}
	poetry := []map[string]any{py.Tool.Poetry.Dependencies}
	for _, g := range py.Tool.Poetry.Group {
		poetry = append(poetry, g.Dependencies)
	}
	for _, group := range poetry {
		for name, spec := range group {
			if normalizePy(name) != id.pyName {
				continue
			}
			table, isTable := spec.(map[string]any)
			switch {
			case isTable && table["path"] != nil:
				out = append(out, proposal(LinkPath, fmt.Sprintf("pyproject.toml poetry %s path = %v", name, table["path"])))
			case isTable && table["git"] != nil:
				out = append(out, proposal(LinkNone, fmt.Sprintf("pyproject.toml poetry %s git = %v has no version", name, table["git"])))
			default:
				out = append(out, proposal(LinkPackage, fmt.Sprintf("pyproject.toml poetry %s = %v", name, spec)))
			}
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
		url := strings.TrimSpace(value)
		if r, err := ParseRemote(url); err == nil {
			if strings.EqualFold(r, repo) {
				return proposal(LinkSubmodule, ".gitmodules has "+url), true
			}
			continue
		}
		// A relative URL (../component.git) names a sibling on the same host.
		if m := relRemote.FindStringSubmatch(url); m != nil && strings.EqualFold(m[1], path.Base(repo)) {
			return proposal(LinkSubmodule, ".gitmodules has "+url), true
		}
	}
	return Proposal{}, false
}

// ParseRemote returns the owner/repo of a git remote URL: scheme://host/…
// or user@host:…. Local paths are not remotes.
func ParseRemote(url string) (string, error) {
	m := remoteRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", fmt.Errorf("%q is not a remote repository URL", url)
	}
	return m[1], nil
}
