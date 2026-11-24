package config_test

import (
	"os"
	"strings"
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
