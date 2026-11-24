package runner_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/vcs"
)

const description = "## Summary\n\nA cache.\n\n## Evidence\n\n- Before: slow. After: fast.\n\n## Merge Danger\n\n**Door:** two-way\n\n**Blast Radius:** cache\n"

func TestADescriptionNeedsItsSectionsAndADoor(t *testing.T) {
	if err := runner.CheckDescription(description); err != nil {
		t.Fatalf("a full description: %v", err)
	}
	for name, body := range map[string]string{
		"no summary":  strings.Replace(description, "## Summary", "## Overview", 1),
		"no evidence": strings.Replace(description, "## Evidence", "## Proof", 1),
		"no danger":   strings.Split(description, "## Merge Danger")[0],
		"no door":     strings.Replace(description, "two-way", "small", 1),
	} {
		if err := runner.CheckDescription(body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func prFixture(t *testing.T, pipelineToml string) (*cpFixture, *githubtest.Server, *runner.PullRequests) {
	t.Helper()
	f := newCheckpointsWith(t, pipelineToml)
	gh := githubtest.New(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	return f, gh, runner.NewPullRequests(f.cp, c, vcs.NewWatcher(c, s, nil))
}

func TestPrOpenIsRefusedWhenTheFullChecksFail(t *testing.T) {
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"echo FULL SUITE BROKE; exit 1\"\n")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), "FULL SUITE BROKE") {
		t.Fatalf("pr open with red full checks: %v", err)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
}

func TestPrOpenPushesAndOpensADraftLinkedToTheTicket(t *testing.T) {
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"true\"\n")
	write(t, filepath.Join(f.ws.Dir, "cache.go"), "package cache\n")
	gitIn(t, f.ws.Dir, "add", ".")
	gitIn(t, f.ws.Dir, "commit", "-q", "-m", "cache")
	pr, err := prs.Open(ctx, f.run, "Add the cache", description)
	if err != nil {
		t.Fatalf("pr open: %v", err)
	}
	got := gh.Issue(pr.Number)
	if !got.Draft || got.Branch != f.ws.Branch || !strings.Contains(got.Body, "Ticket: "+f.tk.Ref) || !strings.Contains(got.Body, "## Merge Danger") {
		t.Fatalf("pull request = %+v", got)
	}
	if f.head() != strings.TrimSpace(gitIn(t, f.ws.Dir, "rev-parse", "HEAD")) {
		t.Fatal("the branch was not pushed before the pull request")
	}
}
