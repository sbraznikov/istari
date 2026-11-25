package runner_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
)

const description = "## Summary\n\nA cache.\n\n## Evidence\n\n- Before: slow. After: fast.\n\n## Merge Danger\n\n**Door:** two-way\n\n**Blast Radius:** cache\n"

func TestADescriptionNeedsItsSectionsAndADoor(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"echo FULL SUITE BROKE; exit 1\"\n")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), "FULL SUITE BROKE") {
		t.Fatalf("pr open with red full checks: %v", err)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
}

func TestPrOpenPushesAndOpensADraftLinkedToTheTicket(t *testing.T) {
	t.Parallel()
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

func TestPrOpenIsRefusedWhileTheWorktreeHasUncommittedChanges(t *testing.T) {
	t.Parallel()
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"true\"\n")
	write(t, filepath.Join(f.ws.Dir, "cache.go"), "package cache\n")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), "istari checkpoint") {
		t.Fatalf("pr open with uncommitted work: %v", err)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
}

func TestPrOpenJournalsTheFullChecksItRan(t *testing.T) {
	t.Parallel()
	f, _, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"echo FULL SUITE BROKE; exit 1\"\n")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil {
		t.Fatal("pr open with red full checks was accepted")
	}
	entries := prJournal(t, f)
	if len(entries) != 1 || entries[0].Checkpoint == nil || entries[0].Checkpoint.Checks["full"] != "red" || !strings.Contains(entries[0].Checkpoint.Failures["full"], "FULL SUITE BROKE") {
		t.Fatalf("journal = %+v", entries)
	}
}

func TestPrOpenWithoutAnyCheckSaysSoInTheJournal(t *testing.T) {
	t.Parallel()
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\n")
	pr, err := prs.Open(ctx, f.run, "Add the cache", description)
	if err != nil {
		t.Fatalf("pr open: %v", err)
	}
	entries := prJournal(t, f)
	if len(entries) != 1 || entries[0].Checkpoint == nil || !strings.Contains(strings.Join(entries[0].Checkpoint.Notes, "\n"), "full: no command") ||
		!strings.Contains(entries[0].Checkpoint.Done, fmt.Sprintf("#%d", pr.Number)) || gh.IssueCount() != 1 {
		t.Fatalf("journal = %+v", entries)
	}
}

func TestARedCheckAtPRKeepsThePullRequestClosed(t *testing.T) {
	t.Parallel()
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"true\"\nformat = \"true\"\ntidy = { run = \"echo GO.MOD NOT TIDY; exit 1\", at = [\"pr\"] }\n")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), "GO.MOD NOT TIDY") {
		t.Fatalf("pr open with a red tidy check: %v", err)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
	entries := prJournal(t, f)
	if len(entries) != 1 || entries[0].Checkpoint == nil {
		t.Fatalf("journal = %+v", entries)
	}
	if c := entries[0].Checkpoint; c.Checks["tidy"] != "red" || c.Checks["full"] != "green" || c.Checks["format"] != "green" ||
		!strings.Contains(c.Failures["tidy"], "GO.MOD NOT TIDY") {
		t.Fatalf("checks = %v failures %v, want tidy red, full and format green", c.Checks, c.Failures)
	}
}

func prJournal(t *testing.T, f *cpFixture) []journal.Entry {
	t.Helper()
	ref, err := tickets.ParseRef(f.tk.Ref)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := journal.New(nil, f.store, nil).Stored(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestPrOpenInALaterRoundUpdatesTheTicketsPullRequest(t *testing.T) {
	t.Parallel()
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"true\"\n")
	first, err := prs.Open(ctx, f.run, "Add the cache", description)
	if err != nil {
		t.Fatalf("pr open: %v", err)
	}
	write(t, filepath.Join(f.ws.Dir, "cache.go"), "package cache\n")
	gitIn(t, f.ws.Dir, "add", ".")
	gitIn(t, f.ws.Dir, "commit", "-q", "-m", "rework")
	again, err := prs.Open(ctx, f.run, "Add the cache, reworked", strings.Replace(description, "A cache.", "A reworked cache.", 1))
	if err != nil {
		t.Fatalf("pr open in the rework round: %v", err)
	}
	if again.Number != first.Number || gh.IssueCount() != 1 || !strings.Contains(gh.Issue(first.Number).Body, "A reworked cache.") {
		t.Fatalf("first %+v, again %+v, pull request %+v", first, again, gh.Issue(first.Number))
	}
	if f.head() != strings.TrimSpace(gitIn(t, f.ws.Dir, "rev-parse", "HEAD")) {
		t.Fatal("the rework was not pushed")
	}
}
