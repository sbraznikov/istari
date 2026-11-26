package runner_test

import (
	"fmt"
	"os"
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
	return f, gh, runner.NewPullRequests(f.cp, c, vcs.NewWatcher(c, s, vcs.WatcherOptions{}), f.tickets)
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

// loggedPrFixture runs full as the full check, logging the directory of each
// run; runs returns the directories logged since its last call.
func loggedPrFixture(t *testing.T, full string) (f *cpFixture, gh *githubtest.Server, prs *runner.PullRequests, runs func() []string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "runs.log")
	f, gh, prs = prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"pwd -P >> "+log+"; "+full+"\"\n")
	runs = func() []string {
		data, _ := os.ReadFile(log)
		os.Remove(log)
		return strings.Fields(string(data))
	}
	return f, gh, prs, runs
}

func TestASecondPrOpenOnTheSameHeadAfterARedFullIsRefusedWithoutRunningAnyCheck(t *testing.T) {
	t.Parallel()
	f, gh, prs, runs := loggedPrFixture(t, "test ! -e broken || { echo FULL SUITE BROKE; exit 1; }")
	write(t, filepath.Join(f.ws.Dir, "broken"), "x\n")
	gitIn(t, f.ws.Dir, "add", ".")
	gitIn(t, f.ws.Dir, "commit", "-q", "-m", "break it")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil {
		t.Fatal("pr open with red full checks was accepted")
	}
	runs()
	head := strings.TrimSpace(gitIn(t, f.ws.Dir, "rev-parse", "--short=12", "HEAD"))
	_, err := prs.Open(ctx, f.run, "Add the cache", description)
	if err == nil || !strings.Contains(err.Error(), head) || !strings.Contains(err.Error(), "change the code") || !strings.Contains(err.Error(), "merge") {
		t.Fatalf("a second pr open on %s: %v", head, err)
	}
	if got := runs(); len(got) != 0 {
		t.Fatalf("the second pr open ran the full check in %v", got)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
}

func TestARedFullThatIsGreenOnMainIsTheTicketsFailure(t *testing.T) {
	t.Parallel()
	f, gh, prs, runs := loggedPrFixture(t, "test ! -e broken || { echo FULL SUITE BROKE; exit 1; }")
	write(t, filepath.Join(f.ws.Dir, "broken"), "x\n")
	gitIn(t, f.ws.Dir, "add", ".")
	gitIn(t, f.ws.Dir, "commit", "-q", "-m", "break it")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), "FULL SUITE BROKE") {
		t.Fatalf("pr open with red full checks: %v", err)
	}
	if got := f.elsewhere(t, runs()); len(got) != 1 {
		t.Fatalf("the full check ran %d times outside the ticket's worktree (%v), want once on main", len(got), got)
	}
	main := strings.TrimSpace(gitIn(t, filepath.Join(f.root, "remotes", "acme", "web.git"), "rev-parse", "--short=12", "main"))
	entries := prJournal(t, f)
	last := entries[len(entries)-1].Checkpoint
	if last == nil || last.Main == nil || last.Main.Commit != main || last.Main.Result != "green" {
		t.Fatalf("the entry = %+v, want main %s green", last, main)
	}
	if tk, err := f.tickets.Get(ctx, f.tk.Ref); err != nil || tk.Hold != "" {
		t.Fatalf("ticket = %+v, %v; want it not held", tk, err)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
}

func TestAFullThatFailsOnMainOnceAndPassesOnTheSecondRunRaisesNoQuestion(t *testing.T) {
	t.Parallel()
	flaked := filepath.Join(t.TempDir(), "flaked")
	f, gh, prs, runs := loggedPrFixture(t, "test ! -e broken || { echo FULL SUITE BROKE; exit 1; }; test -e "+flaked+" || { touch "+flaked+"; echo MAIN FLAKE; exit 1; }")
	write(t, filepath.Join(f.ws.Dir, "broken"), "x\n")
	gitIn(t, f.ws.Dir, "add", ".")
	gitIn(t, f.ws.Dir, "commit", "-q", "-m", "break it")
	_, err := prs.Open(ctx, f.run, "Add the cache", description)
	if err == nil || !strings.Contains(err.Error(), "FULL SUITE BROKE") || strings.Contains(err.Error(), "end your run") {
		t.Fatalf("pr open with a full check that flaked on main: %v", err)
	}
	if got := f.elsewhere(t, runs()); len(got) != 2 {
		t.Fatalf("the full check ran %d times outside the ticket's worktree (%v), want twice on main", len(got), got)
	}
	main := strings.TrimSpace(gitIn(t, filepath.Join(f.root, "remotes", "acme", "web.git"), "rev-parse", "--short=12", "main"))
	entries := prJournal(t, f)
	for _, e := range entries {
		if e.Kind == journal.Question {
			t.Fatalf("a question was raised: %q", e.Text)
		}
	}
	if last := entries[len(entries)-1].Checkpoint; last == nil || last.Main == nil || last.Main.Commit != main || last.Main.Result != "green" {
		t.Fatalf("the entry = %+v, want main %s green", last, main)
	}
	if tk, err := f.tickets.Get(ctx, f.tk.Ref); err != nil || tk.Hold != "" {
		t.Fatalf("ticket = %+v, %v; want it not held", tk, err)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
}

func TestTheFullCheckRunsOnMainOncePerMainCommit(t *testing.T) {
	t.Parallel()
	f, _, prs, runs := loggedPrFixture(t, "test ! -e broken || { echo FULL SUITE BROKE; exit 1; }")
	for _, attempt := range []string{"break it", "try again"} {
		write(t, filepath.Join(f.ws.Dir, "broken"), attempt+"\n")
		gitIn(t, f.ws.Dir, "add", ".")
		gitIn(t, f.ws.Dir, "commit", "-q", "-m", attempt)
		if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), "FULL SUITE BROKE") {
			t.Fatalf("%s: pr open: %v", attempt, err)
		}
	}
	all := runs()
	if got := f.elsewhere(t, all); len(got) != 1 || len(all) != 3 {
		t.Fatalf("runs = %v; want two on the branch and one on main", all)
	}
	entries := prJournal(t, f)
	if last := entries[len(entries)-1].Checkpoint; last == nil || last.Main == nil || last.Main.Result != "green" {
		t.Fatalf("the second entry = %+v, want main's result again", last)
	}
}

func TestARedFullThatFailsOnMainTooHoldsTheTicketWithAQuestion(t *testing.T) {
	t.Parallel()
	f, gh, prs, runs := loggedPrFixture(t, "echo FULL SUITE BROKE; exit 1")
	_, err := prs.Open(ctx, f.run, "Add the cache", description)
	if err == nil || !strings.Contains(err.Error(), "end your run") {
		t.Fatalf("pr open with a full check red on main too: %v", err)
	}
	if got := f.elsewhere(t, runs()); len(got) != 2 {
		t.Fatalf("the full check ran %d times outside the ticket's worktree (%v), want twice on main", len(got), got)
	}
	main := strings.TrimSpace(gitIn(t, filepath.Join(f.root, "remotes", "acme", "web.git"), "rev-parse", "--short=12", "main"))
	entries := prJournal(t, f)
	var cp *journal.CheckpointEntry
	var question string
	for _, e := range entries {
		if e.Checkpoint != nil {
			cp = e.Checkpoint
		}
		if e.Kind == journal.Question {
			question = e.Text
		}
	}
	if cp == nil || cp.Main == nil || cp.Main.Commit != main || cp.Main.Result != "red" || !strings.Contains(cp.Main.Output, "FULL SUITE BROKE") ||
		!strings.Contains(strings.Join(cp.Notes, "\n"), main) {
		t.Fatalf("the entry = %+v, want main %s red with its output", cp, main)
	}
	if question != "main fails `full` at "+main {
		t.Fatalf("question = %q", question)
	}
	if tk, err := f.tickets.Get(ctx, f.tk.Ref); err != nil || tk.Hold != "needs_input" {
		t.Fatalf("ticket = %+v, %v; want it held needs_input", tk, err)
	}
	if gh.IssueCount() != 0 {
		t.Fatal("a pull request was opened")
	}
	// The red was not the branch's: a later pr open on the same head, as after
	// the hold is lifted, runs the checks again and reuses main's result.
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err == nil || !strings.Contains(err.Error(), "end your run") {
		t.Fatalf("a later pr open on the same head and main tip: %v", err)
	}
	if all := runs(); len(all) != 1 || len(f.elsewhere(t, all)) != 0 {
		t.Fatalf("a later pr open on the same head and main tip ran the full check in %v, want once on the branch", all)
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
	if len(entries) == 0 || entries[0].Checkpoint == nil || entries[0].Checkpoint.Checks["full"] != "red" || !strings.Contains(entries[0].Checkpoint.Failures["full"], "FULL SUITE BROKE") {
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

func TestACheckOfTheSlicesTestsDoesNotRunBeforeAPullRequest(t *testing.T) {
	t.Parallel()
	f, gh, prs := prFixture(t, "[checks]\ntypecheck = \"true\"\ntest = \"exit 1 {dirs}\"\nfull = \"true\"\nnames = \"exit 1 {files}\"\n")
	if _, err := prs.Open(ctx, f.run, "Add the cache", description); err != nil {
		t.Fatalf("pr open: %v", err)
	}
	entries := prJournal(t, f)
	if len(entries) != 1 || gh.IssueCount() != 1 {
		t.Fatalf("journal = %+v", entries)
	}
	if c := entries[0].Checkpoint; c.Checks["full"] != "green" || c.Checks["names"] != "" || !strings.Contains(strings.Join(c.Notes, " "), "names: not run") {
		t.Fatalf("checks = %v notes %v, want names left to the slices and noted", c.Checks, c.Notes)
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
