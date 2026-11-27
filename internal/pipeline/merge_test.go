package pipeline_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
)

// github is what the merge stage asks of GitHub, as a test sets it.
type github struct {
	approvedBy string   // who approved the head; "": nobody
	merges     []string // the methods istari merged with
	asked      int      // how often the approval was asked
	humanMerge bool     // a human merged the pull request
	ci         string   // the head's CI state
	findings   int      // the review's accepted findings
	changes    vcs.Changes
}

func (g *github) merged(context.Context, string) (bool, error) { return g.humanMerge, nil }

func (g *github) checks(context.Context, string) (string, bool) { return g.ci, g.ci != "" }

func (g *github) review(context.Context, string, string) (int, bool, error) {
	return g.findings, true, nil
}

func (g *github) changesOf(context.Context, string) (vcs.Changes, error) { return g.changes, nil }

func (g *github) approval(context.Context, string) (string, error) {
	g.asked++
	return g.approvedBy, nil
}

func (g *github) merge(_ context.Context, _, method string) error {
	g.merges = append(g.merges, method)
	return nil
}

// mergeFixture is an engine whose merge stage talks to gh.
type mergeFixture struct {
	*engineFixture
	gh      *github
	journal *journal.Journal
}

func newMergeEngine(t *testing.T, defaults config.Defaults, repoFiles files) *mergeFixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	gh := &github{ci: "success", changes: vcs.Changes{Head: "h1", Paths: []string{"web/page.go"}, Files: 1, Lines: 20}}
	j := journal.New(nil, s, nil)
	f := &engineFixture{store: s, tasks: core.NewTasks(s, core.TaskOptions{}), tickets: core.NewTickets(s, core.TicketOptions{})}
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{
		Defaults: defaults, Files: repoFiles, Journal: j, Log: s,
		Approval: gh.approval, Merge: gh.merge, Merged: gh.merged,
		Checks: gh.checks, Review: gh.review, Changes: gh.changesOf,
	})
	return &mergeFixture{engineFixture: f, gh: gh, journal: j}
}

// awaitingApproval brings the ticket through plan, implement, verify and
// review to awaiting_approval.
func (f *mergeFixture) awaitingApproval(t *testing.T) {
	t.Helper()
	f.readyTicket(t, ref)
	if _, err := f.tickets.SetSize(ctx, "agent:planner-1", ref, "S"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1")
	if _, err := f.tickets.Fire(ctx, "agent:implementer-1", ref, "pr_ready"); err != nil {
		t.Fatal(err)
	}
	f.finish(t, ref, pipeline.StageVerify, "system")
	f.reconcile(t)
	f.finish(t, ref, "review", "agent:reviewer-1")
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketAwaitingApproval {
		t.Fatalf("the ticket is %s, want awaiting_approval", st)
	}
}

func merging(mode string) config.Defaults {
	return config.Defaults{Merge: config.Merge{Mode: mode, Method: "rebase", AutoMaxSize: "S",
		AutoMaxLines: map[string]int{"XS": 50, "S": 200}, AutoMaxFiles: map[string]int{"XS": 3, "S": 10}}}
}

func TestInAutoModeAPullRequestThatMeetsEveryConditionMergesWithoutAHuman(t *testing.T) {
	t.Parallel()
	f := newMergeEngine(t, merging("auto"), files{"acme/web": "[merge]\nirreversible_paths = [\"migrations/**\"]\n"})
	f.awaitingApproval(t)
	f.reconcile(t)
	if f.gh.asked != 0 {
		t.Fatalf("the approval was asked %d times, want none in auto mode", f.gh.asked)
	}
	if len(f.gh.merges) != 1 || f.gh.merges[0] != "rebase" {
		t.Fatalf("merges = %v, want one with rebase", f.gh.merges)
	}
	if st := f.state(t, ref); st != core.TicketDone {
		t.Fatalf("the ticket is %s, want done", st)
	}
}

func TestAfterApprovalAnOwnersApprovalOfTheHeadMergesWithTheMethod(t *testing.T) {
	t.Parallel()
	f := newMergeEngine(t, merging("after_approval"), nil)
	f.awaitingApproval(t)
	f.gh.approvedBy = "kim"
	f.reconcile(t)
	if len(f.gh.merges) != 1 || f.gh.merges[0] != "rebase" {
		t.Fatalf("merges = %v, want one with the configured method rebase", f.gh.merges)
	}
	if st := f.state(t, ref); st != core.TicketDone {
		t.Fatalf("after the merge the ticket is %s, want done (T11)", st)
	}
	for _, stage := range []string{"approve", pipeline.StageMerge} {
		if task := f.stage(t, ref, stage); task.State != core.TaskCompleted {
			t.Fatalf("%s is %s, want completed", stage, task.State)
		}
	}
}

func TestInAutoModeEachFailedConditionLeavesThePullRequestToAHumanNamingIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		condition string
		fails     func(t *testing.T, f *mergeFixture)
		reason    string
	}{
		{pipeline.CondSize, func(t *testing.T, f *mergeFixture) {
			if _, err := f.tickets.SetSize(ctx, "human:alex", ref, "M"); err != nil {
				t.Fatal(err)
			}
		}, "the ticket is M, larger than auto_max_size S"},
		{pipeline.CondReview, func(_ *testing.T, f *mergeFixture) { f.gh.findings = 1 }, "the review left open findings (1)"},
		{pipeline.CondCI, func(_ *testing.T, f *mergeFixture) { f.gh.ci = "failure" }, `CI is "failure", not green`},
		{pipeline.CondIrreversible, func(_ *testing.T, f *mergeFixture) {
			// The description says the change is reversible: it cannot relax what code finds.
			f.gh.changes.Paths = []string{"web/page.go", "migrations/2026/001.sql"}
			f.gh.changes.MergeDanger = "Two-way door; blast radius: none."
		}, "the diff touches migrations/2026/001.sql, an irreversible path (migrations/**)"},
		{pipeline.CondMergeDanger, func(_ *testing.T, f *mergeFixture) {
			f.gh.changes.MergeDanger = "One-way door; blast radius: data."
		}, "the Merge Danger section calls the change a one-way door"},
		{pipeline.CondLines, func(_ *testing.T, f *mergeFixture) { f.gh.changes.Lines = 201 }, "the diff changes 201 lines, more than the 200 of size S"},
		{pipeline.CondFiles, func(_ *testing.T, f *mergeFixture) { f.gh.changes.Files = 11 }, "the diff changes 11 files, more than the 10 of size S"},
		{pipeline.CondAssumptions, func(t *testing.T, f *mergeFixture) {
			r, _ := tickets.ParseRef(ref)
			if err := f.journal.Append(ctx, r, journal.Entry{Kind: journal.Assumption, Actor: "agent:implementer-1", Text: "the cache keeps an hour"}); err != nil {
				t.Fatal(err)
			}
		}, "assumptions recorded during the work await a human's verification (1)"},
	} {
		t.Run(tc.condition, func(t *testing.T) {
			t.Parallel()
			f := newMergeEngine(t, merging("auto"), files{"acme/web": "[merge]\nirreversible_paths = [\"migrations/**\"]\n"})
			f.awaitingApproval(t)
			tc.fails(t, f)
			f.reconcile(t)
			f.reconcile(t)
			if len(f.gh.merges) != 0 {
				t.Fatalf("merges = %v, want none: the pull request waits for a human", f.gh.merges)
			}
			if st := f.state(t, ref); st != core.TicketAwaitingApproval {
				t.Fatalf("the ticket is %s, want awaiting_approval", st)
			}
			var notes []string
			r, _ := tickets.ParseRef(ref)
			entries, _ := f.journal.Stored(ctx, r)
			for _, en := range entries {
				if en.Kind == journal.Note && strings.HasPrefix(en.Text, "auto-merge refused") {
					notes = append(notes, en.Text)
				}
			}
			if len(notes) != 1 || !strings.Contains(notes[0], "at h1: "+tc.reason+". ") {
				t.Fatalf("notes = %q, want one naming %q at h1", notes, tc.reason)
			}
			events, _ := f.store.Read(ctx, "", store.Filter{Ticket: ref, TypePrefix: pipeline.EventAutoMergeRefused}, 0)
			if len(events) != 1 {
				t.Fatalf("%d %s events, want one", len(events), pipeline.EventAutoMergeRefused)
			}
			var payload struct {
				Head    string
				Refused []pipeline.Refusal
			}
			if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Head != "h1" || len(payload.Refused) != 1 || payload.Refused[0] != (pipeline.Refusal{Condition: tc.condition, Reason: tc.reason}) {
				t.Fatalf("event = %+v, want only %s at h1", payload, tc.condition)
			}
			f.gh.approvedBy = "kim" // a human approves, as in after_approval
			f.reconcile(t)
			if len(f.gh.merges) != 1 || f.state(t, ref) != core.TicketDone {
				t.Fatalf("after a human's approval: merges %v, ticket %s; want merged and done", f.gh.merges, f.state(t, ref))
			}
		})
	}
}

func TestAfterApprovalWithoutAnApprovalNothingIsMerged(t *testing.T) {
	t.Parallel()
	f := newMergeEngine(t, merging("after_approval"), nil)
	f.awaitingApproval(t)
	f.reconcile(t)
	f.reconcile(t)
	if len(f.gh.merges) != 0 {
		t.Fatalf("merges = %v, want none without an approval", f.gh.merges)
	}
	if st := f.state(t, ref); st != core.TicketAwaitingApproval {
		t.Fatalf("the ticket is %s, want awaiting_approval", st)
	}
}

func TestInManualModeIstariNeitherAsksNorMergesAndAHumansMergeFinishesTheTicket(t *testing.T) {
	t.Parallel()
	f := newMergeEngine(t, merging("manual"), nil)
	f.awaitingApproval(t)
	f.gh.approvedBy = "kim"
	f.reconcile(t)
	if f.gh.asked != 0 || len(f.gh.merges) != 0 {
		t.Fatalf("manual mode: approval asked %d times, merges %v; want neither", f.gh.asked, f.gh.merges)
	}
	if st := f.state(t, ref); st != core.TicketAwaitingApproval {
		t.Fatalf("the ticket is %s, want awaiting_approval until the human merges", st)
	}
	f.gh.humanMerge = true
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketDone {
		t.Fatalf("after the human's merge the ticket is %s, want done (T11)", st)
	}
}
