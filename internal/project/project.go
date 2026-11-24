package project

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/pipeline"
)

// ErrNotGit means a directory is not inside a git repository.
var ErrNotGit = errors.New("not inside a git repository")

// Root returns the top directory of the git repository dir is in.
func Root(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", dir, ErrNotGit)
	}
	return strings.TrimSpace(string(out)), nil
}

// Remote returns the owner/repo of the origin remote of the repository at dir.
func Remote(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("%s has no origin remote: add one with git remote add origin <url>", dir)
	}
	return ParseRemote(string(out))
}

// Resolve finds the project of the repository dir is in, by its origin.
// Every repository of a project resolves to that project.
func Resolve(cfg *config.Config, dir string) (config.Project, error) {
	root, err := Root(dir)
	if err != nil {
		return config.Project{}, err
	}
	repo, err := Remote(root)
	if err != nil {
		return config.Project{}, err
	}
	if p, ok := ByRepo(cfg, repo); ok {
		return p, nil
	}
	return config.Project{}, fmt.Errorf("%s (%s) belongs to no project; run istari init there, or istari repo add from a project's repository", root, repo)
}

// ByRepo finds the project a repository belongs to.
func ByRepo(cfg *config.Config, repo string) (config.Project, bool) {
	for _, p := range cfg.Projects {
		for _, r := range p.RepoList() {
			if strings.EqualFold(r.Repo, repo) {
				return p, true
			}
		}
	}
	return config.Project{}, false
}

//go:embed templates
var templates embed.FS

// Skill returns the agent skill: how to work with istari (spec section 7.4).
func Skill() string {
	data, err := templates.ReadFile("templates/skill.md")
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(data)
}

// Scaffold creates the files of .istari/ under root that do not exist yet:
// the pipeline config, the ticket standard, the role instructions and the
// agent skill. Existing files are left as they are. It returns the paths it
// created, relative to root.
func Scaffold(root string) ([]string, error) {
	var created []string
	err := fs.WalkDir(templates, "templates", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := path.Join(".istari", strings.TrimPrefix(name, "templates/"))
		target := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		body, err := templates.ReadFile(name)
		if err != nil {
			return err
		}
		if rel == ".istari/pipeline.toml" {
			irreversible := ""
			if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
				irreversible = `, "go.mod"`
			}
			body = []byte(strings.NewReplacer("{{checks}}", checksFor(root), "{{irreversible}}", irreversible).Replace(string(body)))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
		created = append(created, rel)
		return nil
	})
	if err != nil {
		return created, err
	}
	// The role instructions: istari's built-in ones, to adapt to the project.
	for _, role := range pipeline.AgentRoles {
		rel := path.Join(".istari", "roles", role+".md")
		target := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(target); err == nil {
			continue
		}
		text, err := pipeline.BuiltInInstruction(role)
		if err != nil {
			return created, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return created, err
		}
		if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
			return created, err
		}
		created = append(created, rel)
	}
	return created, nil
}

// checksFor proposes the check commands of spec section 7.7 for the
// repository's language, or leaves them for a human to fill in.
func checksFor(root string) string {
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(root, name)); return err == nil }
	switch {
	case exists("go.mod"):
		return "typecheck = \"go vet ./...\"\ntest      = \"go test ./... -short\"\nfull      = \"go test ./...\""
	case exists("package.json"):
		return "typecheck = \"npx tsc --noEmit\"\ntest      = \"npm test\"\nfull      = \"npm test\""
	case exists("pyproject.toml"):
		return "typecheck = \"mypy .\"\ntest      = \"pytest -q -x\"\nfull      = \"pytest -q\""
	default:
		return "# typecheck = \"...\"   # fill in: istari runs these itself (spec section 7.7)\n# test      = \"...\"\n# full      = \"...\""
	}
}
