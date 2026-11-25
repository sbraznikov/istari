package project_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sbraznikov/istari/internal/project"
)

// tree writes files under a new directory and returns it.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetectLink(t *testing.T) {
	t.Parallel()
	component := map[string]map[string]string{
		"go":     {"go.mod": "module github.com/acme/component\n\ngo 1.27\n"},
		"npm":    {"package.json": `{"name": "@acme/component", "version": "1.4.0"}`},
		"python": {"pyproject.toml": "[project]\nname = \"acme-component\"\nversion = \"1.4.0\"\n"},
		"bare":   {"README.md": "a component\n"},
	}
	for _, tc := range []struct {
		name       string
		component  string
		service    map[string]string
		repo       string // the component's owner/repo
		link       project.Link
		doneWhen   string
		softUsable bool
	}{
		{"go versioned", "go", map[string]string{"go.mod": "module github.com/acme/service\n\nrequire github.com/acme/component v1.4.0\n"}, "acme/component", project.LinkPackage, "released", true},
		{"go replace", "go", map[string]string{"go.mod": "module github.com/acme/service\n\nrequire github.com/acme/component v0.0.0\n\nreplace github.com/acme/component => ../component\n"}, "acme/component", project.LinkPath, "merged", true},
		{"npm versioned", "npm", map[string]string{"package.json": `{"name": "service", "dependencies": {"@acme/component": "^1.4.0"}}`}, "acme/component", project.LinkPackage, "released", true},
		{"npm file", "npm", map[string]string{"package.json": `{"name": "service", "dependencies": {"@acme/component": "file:../component"}}`}, "acme/component", project.LinkPath, "merged", true},
		{"python versioned", "python", map[string]string{"pyproject.toml": "[project]\nname = \"service\"\ndependencies = [\"acme-component>=1.4\"]\n"}, "acme/component", project.LinkPackage, "released", true},
		{"python path", "python", map[string]string{"pyproject.toml": "[tool.poetry.dependencies]\nacme-component = { path = \"../component\", develop = true }\n"}, "acme/component", project.LinkPath, "merged", true},
		{"submodule", "bare", map[string]string{".gitmodules": "[submodule \"vendor/component\"]\n\tpath = vendor/component\n\turl = git@github.com:acme/component.git\n"}, "acme/component", project.LinkSubmodule, "merged", true},
		{"nothing", "bare", map[string]string{"README.md": "a service\n"}, "acme/component", project.LinkNone, "merged", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := project.DetectLink(tree(t, tc.service), tree(t, component[tc.component]), "acme/service", tc.repo)
			if p.Link != tc.link || p.DoneWhen != tc.doneWhen || p.SoftAvailable() != tc.softUsable || p.Evidence == "" && tc.link != project.LinkNone {
				t.Fatalf("DetectLink = %+v (soft %v), want link %s, done_when %s, soft %v", p, p.SoftAvailable(), tc.link, tc.doneWhen, tc.softUsable)
			}
		})
	}
}

func TestDetectLinkLooksBothWays(t *testing.T) {
	t.Parallel()
	// The component depends on the service: still a published package.
	service := tree(t, map[string]string{"go.mod": "module github.com/acme/service\n"})
	component := tree(t, map[string]string{"go.mod": "module github.com/acme/component\n\nrequire github.com/acme/service v2.0.1\n"})
	if p := project.DetectLink(service, component, "acme/service", "acme/component"); p.Link != project.LinkPackage {
		t.Fatalf("DetectLink = %+v, want a package link found in the added repository", p)
	}
}

func TestParseRemote(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"git@github.com:acme/web.git":           "acme/web",
		"https://github.com/acme/web.git":       "acme/web",
		"https://github.com/acme/web":           "acme/web",
		"ssh://git@github.com/acme/web.git":     "acme/web",
		"https://x-token@github.com/Acme/Web-2": "Acme/Web-2",
	} {
		if got, err := project.ParseRemote(in); err != nil || got != want {
			t.Errorf("ParseRemote(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := project.ParseRemote("/local/path/repo"); err == nil {
		t.Error("ParseRemote accepted a local path")
	}
}

func TestDetectLinkEdgeCases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		primary, added map[string]string
		link           project.Link
		doneWhen       string
	}{
		{"npm git reference has no version", map[string]string{"package.json": `{"dependencies": {"@acme/component": "github:acme/component#main"}}`},
			map[string]string{"package.json": `{"name": "@acme/component"}`}, project.LinkNone, "merged"},
		{"pep 508 git reference has no version", map[string]string{"pyproject.toml": "[project]\ndependencies = [\"acme-component @ git+https://github.com/acme/component\"]\n"},
			map[string]string{"pyproject.toml": "[project]\nname = \"acme-component\"\n"}, project.LinkNone, "merged"},
		{"uv path source", map[string]string{"pyproject.toml": "[project]\ndependencies = [\"acme-component\"]\n\n[tool.uv.sources]\nacme-component = { path = \"../component\" }\n"},
			map[string]string{"pyproject.toml": "[project]\nname = \"acme-component\"\n"}, project.LinkPath, "merged"},
		{"poetry 2 sets both names", map[string]string{"pyproject.toml": "[project]\ndependencies = [\"acme-component>=1.0\"]\n"},
			map[string]string{"pyproject.toml": "[project]\nname = \"acme-component\"\n\n[tool.poetry]\nname = \"acme-component\"\n"}, project.LinkPackage, "released"},
		{"relative submodule url", map[string]string{".gitmodules": "[submodule \"c\"]\n\tpath = c\n\turl = ../component.git\n"},
			nil, project.LinkSubmodule, "merged"},
		{"the added repository consumes the primary's package", map[string]string{"go.mod": "module github.com/acme/service\n"},
			map[string]string{"go.mod": "module github.com/acme/component\n\nrequire github.com/acme/service v2.0.1\n"}, project.LinkPackage, "merged"},
		{"the added repository has the primary as a submodule", nil,
			map[string]string{".gitmodules": "[submodule \"s\"]\n\turl = git@github.com:acme/service.git\n"}, project.LinkSubmodule, "merged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := project.DetectLink(tree(t, tc.primary), tree(t, tc.added), "acme/service", "acme/component")
			if p.Link != tc.link || p.DoneWhen != tc.doneWhen {
				t.Fatalf("DetectLink = %+v, want %s / %s", p, tc.link, tc.doneWhen)
			}
		})
	}
}

func TestParseRemoteRejectsLocalPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"mirrors/acme/web", "../component.git", "acme/web"} {
		if got, err := project.ParseRemote(path); err == nil {
			t.Errorf("ParseRemote(%q) = %q, want an error", path, got)
		}
	}
}
