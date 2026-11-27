package main_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo creates a git repository whose origin is remote, with files.
func gitRepo(t *testing.T, remote string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// istariIn runs istari with the config in dir.
func istariIn(t *testing.T, dir, config string, args ...string) (string, string, int) {
	t.Helper()
	return istariEnv(t, dir, config, nil, args...)
}

// istariEnv runs istari with the config in dir with extra environment.
func istariEnv(t *testing.T, dir, config string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"--config", config}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return out.String(), errOut.String(), code
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			files[path] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestInitCreatesTheIstariFilesAndRegistersTheProjectOnce(t *testing.T) {
	t.Parallel()
	repo := gitRepo(t, "git@github.com:acme/web.git", nil)
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte("# my installation\n[daemon]\nmax_concurrent_runs = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := istariIn(t, repo, config, "init"); code != 0 {
		t.Fatalf("init: exit %d %s", code, stderr)
	}
	for _, f := range []string{"pipeline.toml", "ticket-standard.toml", "ticket-standard.md", "skill.md",
		"roles/refiner.md", "roles/planner.md", "roles/implementer.md", "roles/reviewer.md"} {
		if _, err := os.Stat(filepath.Join(repo, ".istari", f)); err != nil {
			t.Errorf(".istari/%s was not created: %v", f, err)
		}
	}
	// The role files are the built-in instructions, as a starting point.
	if impl, _ := os.ReadFile(filepath.Join(repo, ".istari", "roles", "implementer.md")); !strings.Contains(string(impl), "Refactoring is not part of the cycle") {
		t.Errorf("the scaffolded implementer instruction is not the built-in one:\n%s", impl)
	}
	cfg, _ := os.ReadFile(config)
	if !strings.Contains(string(cfg), "# my installation") || !strings.Contains(string(cfg), `repo = "acme/web"`) || !strings.Contains(string(cfg), `name = "web"`) {
		t.Fatalf("config after init:\n%s", cfg)
	}

	before := snapshot(t, filepath.Dir(config))
	beforeRepo := snapshot(t, filepath.Join(repo, ".istari"))
	out, stderr, code := istariIn(t, repo, config, "init")
	if code != 0 {
		t.Fatalf("second init: exit %d %s", code, stderr)
	}
	if after := snapshot(t, filepath.Dir(config)); after[config] != before[config] {
		t.Fatalf("a second init changed the config:\n%s", after[config])
	}
	afterRepo := snapshot(t, filepath.Join(repo, ".istari"))
	if len(afterRepo) != len(beforeRepo) {
		t.Fatal("a second init changed .istari/")
	}
	for path, body := range beforeRepo {
		if afterRepo[path] != body {
			t.Fatalf("a second init changed %s", path)
		}
	}
	if !strings.Contains(out, "already") {
		t.Errorf("second init output %q does not say the project exists", out)
	}
}

func TestInitOutsideAGitRepositoryFails(t *testing.T) {
	t.Parallel()
	config := filepath.Join(t.TempDir(), "config.toml")
	if _, stderr, code := istariIn(t, t.TempDir(), config, "init"); code != 1 || !strings.Contains(stderr, "git") {
		t.Fatalf("init outside git: exit %d %q, want 1 mentioning git", code, stderr)
	}
}

type repoList struct {
	Project string
	Repos   []struct {
		Repo     string
		DoneWhen string `json:"done_when"`
		Link     string
		Primary  bool
		Soft     bool
	}
}

func TestRepoAddProposesDoneWhenAndBothRepositoriesResolveToTheProject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		service  map[string]string
		doneWhen string
		link     string
		soft     bool
	}{
		{"versioned", map[string]string{"go.mod": "module github.com/acme/service\n\nrequire github.com/acme/component v1.4.0\n"}, "released", "package", true},
		{"submodule", map[string]string{".gitmodules": "[submodule \"c\"]\n\tpath = c\n\turl = https://github.com/acme/component.git\n"}, "merged", "submodule", true},
		{"local path", map[string]string{"go.mod": "module github.com/acme/service\n\nreplace github.com/acme/component => ../component\n"}, "merged", "path", true},
		{"nothing", nil, "merged", "none", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service := gitRepo(t, "git@github.com:acme/service.git", tc.service)
			component := gitRepo(t, "git@github.com:acme/component.git", map[string]string{"go.mod": "module github.com/acme/component\n"})
			config := filepath.Join(t.TempDir(), "config.toml")
			if _, stderr, code := istariIn(t, service, config, "init"); code != 0 {
				t.Fatalf("init: %s", stderr)
			}
			// Without a terminal or --yes, repo add only proposes.
			out, _, code := istariIn(t, service, config, "repo", "add", component)
			if code != 1 || !strings.Contains(out, "done_when = \""+tc.doneWhen+"\"") {
				t.Fatalf("unconfirmed repo add: exit %d, output %q; want the proposal done_when = %q and exit 1", code, out, tc.doneWhen)
			}
			if _, stderr, code := istariIn(t, service, config, "repo", "add", component, "--yes"); code != 0 {
				t.Fatalf("repo add --yes: exit %d %s", code, stderr)
			}
			for _, dir := range []string{service, component} {
				stdout, stderr, code := istariIn(t, dir, config, "--json", "repo", "list")
				var list repoList
				if code != 0 || json.Unmarshal([]byte(stdout), &list) != nil {
					t.Fatalf("repo list in %s: exit %d %s %s", dir, code, stdout, stderr)
				}
				if list.Project != "service" || len(list.Repos) != 2 {
					t.Fatalf("repo list in %s = %+v, want project service with two repositories", dir, list)
				}
				added := list.Repos[1]
				if !list.Repos[0].Primary || list.Repos[0].Repo != "acme/service" || added.Repo != "acme/component" ||
					added.DoneWhen != tc.doneWhen || added.Link != tc.link || added.Soft != tc.soft {
					t.Fatalf("repositories = %+v, want the service primary and the component with %s/%s, soft %v", list.Repos, tc.doneWhen, tc.link, tc.soft)
				}
			}
		})
	}
}

func TestRepoAddWithAnExplicitChoiceAndRepoRemove(t *testing.T) {
	t.Parallel()
	service := gitRepo(t, "git@github.com:acme/service.git", nil)
	component := gitRepo(t, "git@github.com:acme/component.git", nil)
	config := filepath.Join(t.TempDir(), "config.toml")
	istariIn(t, service, config, "init")
	if _, stderr, code := istariIn(t, service, config, "repo", "add", component, "--done-when", "released"); code != 0 {
		t.Fatalf("repo add --done-when: %s", stderr)
	}
	var list repoList
	stdout, _, _ := istariIn(t, service, config, "--json", "repo", "list")
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("repo list: %v\n%s", err, stdout)
	}
	if len(list.Repos) != 2 || list.Repos[1].DoneWhen != "released" {
		t.Fatalf("repo list = %+v, want the human's released", list)
	}
	if _, _, code := istariIn(t, service, config, "repo", "remove", "acme/service"); code != 1 {
		t.Fatal("the primary repository was removed")
	}
	if _, stderr, code := istariIn(t, service, config, "repo", "remove", "acme/component"); code != 0 {
		t.Fatalf("repo remove: %s", stderr)
	}
	stdout, _, _ = istariIn(t, service, config, "--json", "repo", "list")
	list = repoList{}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("repo list after remove: %v\n%s", err, stdout)
	}
	if len(list.Repos) != 1 || list.Repos[0].Repo != "acme/service" {
		t.Fatalf("after remove: %+v", list)
	}
	if _, _, code := istariIn(t, component, config, "repo", "list"); code != 1 {
		t.Fatal("a removed repository still resolves to the project")
	}
}

func TestRepoAddRunsFromThePrimaryRepository(t *testing.T) {
	t.Parallel()
	service := gitRepo(t, "git@github.com:acme/service.git", nil)
	component := gitRepo(t, "git@github.com:acme/component.git", nil)
	lib := gitRepo(t, "git@github.com:acme/lib.git", nil)
	config := filepath.Join(t.TempDir(), "config.toml")
	if _, stderr, code := istariIn(t, service, config, "init"); code != 0 {
		t.Fatal(stderr)
	}
	if _, stderr, code := istariIn(t, service, config, "repo", "add", component, "--yes"); code != 0 {
		t.Fatal(stderr)
	}
	_, stderr, code := istariIn(t, component, config, "repo", "add", lib, "--yes")
	if code != 1 || !strings.Contains(stderr, "primary") {
		t.Fatalf("repo add from a non-primary repository: exit %d %q", code, stderr)
	}
	stdout, _, code := istariIn(t, service, config, "--json", "repo", "remove", "ACME/Component")
	var res struct{ Removed string }
	if code != 0 || json.Unmarshal([]byte(stdout), &res) != nil || res.Removed != "acme/component" {
		t.Fatalf("repo remove in another case: exit %d %q", code, stdout)
	}
}
