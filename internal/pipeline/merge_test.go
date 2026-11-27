package pipeline_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// github is what the merge stage asks of GitHub, as a test sets it.
type github struct {
	approvedBy string   // who approved the head; "": nobody
	merges     []string // the methods istari merged with
	asked      int      // how often the approval was asked
	humanMerge bool     // a human merged the pull request
}

func (g *github) merged(context.Context, string) (bool, error) { return g.humanMerge, nil }

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
	gh := &github{}
	j := journal.New(nil, s, nil)
	f := &engineFixture{store: s, tasks: core.NewTasks(s, core.TaskOptions{}), tickets: core.NewTickets(s, core.TicketOptions{})}
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{
		Defaults: defaults, Files: repoFiles, Journal: j,
		Approval: gh.approval, Merge: gh.merge, Merged: gh.merged,
	})
	return &mergeFixture{engineFixture: f, gh: gh, journal: j}
}

// awaitingApproval brings the ticket through plan, implement, verify and
// review to awaiting_approval.
func (f *mergeFixture) awaitingApproval(t *testing.T) {
	t.Helper()
	f.readyTicket(t, ref)
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
	return config.Defaults{Merge: config.Merge{Mode: mode, Method: "rebase", AutoMaxSize: "S"}}
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
