package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sbraznikov/istari/internal/config"
)

const handwritten = `# my installation
[daemon]
max_concurrent_runs = 2

[[project]]                 # the service
name  = "service"
repo  = "acme/service"
slack = { work = "#svc" }

[[project]]
name = "web"
repo = "acme/web"

[tui]
theme = "terminal"          # keep this comment
`

func TestAddRepoGoesIntoItsProjectAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, handwritten)
	if err := config.AddRepo(path, "service", config.ProjectRepo{Repo: "acme/component", DoneWhen: "released", Link: "package"}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after AddRepo: %v", err)
	}
	svc, web := cfg.Projects[0].RepoList(), cfg.Projects[1].RepoList()
	if len(svc) != 2 || svc[0].Repo != "acme/service" || !svc[0].Primary || svc[1].Repo != "acme/component" || svc[1].DoneWhen != "released" || svc[1].Link != "package" {
		t.Fatalf("service repos = %+v", svc)
	}
	if len(web) != 1 || web[0].Repo != "acme/web" {
		t.Fatalf("web repos = %+v: the edit leaked into the next project", web)
	}
	if cfg.TUI.Theme != "terminal" || cfg.Projects[0].Slack.Work != "#svc" {
		t.Fatalf("other settings changed: %+v %+v", cfg.TUI, cfg.Projects[0].Slack)
	}
	text, _ := os.ReadFile(path)
	for _, c := range []string{"# my installation", "# the service", "# keep this comment"} {
		if !strings.Contains(string(text), c) {
			t.Errorf("comment %q was lost:\n%s", c, text)
		}
	}

	if err := config.AddRepo(path, "service", config.ProjectRepo{Repo: "acme/docs", Link: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := config.RemoveRepo(path, "service", "acme/component"); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	cfg, _ = config.Load(path)
	if got := cfg.Projects[0].RepoList(); len(got) != 2 || got[1].Repo != "acme/docs" {
		t.Fatalf("after remove: %+v", got)
	}
	if err := config.AddRepo(path, "nope", config.ProjectRepo{Repo: "x/y"}); err == nil {
		t.Fatal("AddRepo to a missing project succeeded")
	}
}

func TestAnEditThatWouldBreakTheConfigIsNotWritten(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, handwritten)
	before, _ := os.ReadFile(path)
	if err := config.AddRepo(path, "service", config.ProjectRepo{Repo: "acme/x", DoneWhen: "someday"}); err == nil {
		t.Fatal("an invalid done_when was written")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("a refused edit changed the file")
	}
}

func TestEditsKeepUnusualButValidFiles(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"single quotes":                "[[project]]\nname = 'service'\nrepo = 'acme/service'\n",
		"crlf":                         "[[project]]\r\nname = \"service\"\r\nrepo = \"acme/service\"\r\n",
		"no newline":                   "[[project]]\nname = \"service\"\nrepo = \"acme/service\"",
		"comment above the next table": "[[project]]\nname = \"service\"\nrepo = \"acme/service\"\n\n# --- UI ---\n[tui]\ntheme = \"terminal\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeConfig(t, text)
			if err := config.AddRepo(path, "service", config.ProjectRepo{Repo: "acme/component"}); err != nil {
				t.Fatalf("AddRepo: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.Projects[0].RepoList()
			if len(got) != 2 || got[0].Repo != "acme/service" || !got[0].Primary || got[1].Repo != "acme/component" {
				t.Fatalf("repos = %+v", got)
			}
			out, _ := os.ReadFile(path)
			if strings.Contains(name, "comment") && !strings.Contains(string(out), "# --- UI ---\n[tui]") {
				t.Fatalf("the comment that heads [tui] moved:\n%s", out)
			}
			if name == "crlf" && strings.Count(string(out), "\r\n") != strings.Count(string(out), "\n") {
				t.Fatalf("mixed line endings:\n%q", out)
			}
		})
	}
}

func TestEditsFollowASymlinkedConfig(t *testing.T) {
	t.Parallel()
	real := writeConfig(t, handwritten)
	link := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := config.AddRepo(link, "web", config.ProjectRepo{Repo: "acme/docs"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if text, _ := os.ReadFile(real); !strings.Contains(string(text), "acme/docs") {
		t.Fatal("the edit did not reach the linked file")
	}
}

func TestConcurrentEditsAreNotLost(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, handwritten)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := config.AddRepo(path, "web", config.ProjectRepo{Repo: fmt.Sprintf("acme/r%d", i)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Projects[1].RepoList(); len(got) != 9 {
		t.Fatalf("web has %d repositories after 8 concurrent adds, want 9", len(got))
	}
}

func TestAProjectWithBothRepoAndReposIsInvalid(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "[[project]]\nname = \"s\"\nrepo = \"a/b\"\n\n[[project.repos]]\nrepo = \"a/c\"\n")
	_, err := config.Load(path)
	assertErrorNames(t, err, "project[0].repo")
}
