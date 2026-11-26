package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
)

var ctx = context.Background()

// files serves pipeline files by repository; a repository without one has
// the default pipeline.
type files map[string]string

func (f files) ReadFile(_ context.Context, repo, path string) ([]byte, error) {
	if body, ok := f[repo]; ok && path == pipeline.Path {
		return []byte(body), nil
	}
	return nil, tickets.ErrNotFound
}

type engineFixture struct {
	store   store.Store
	tasks   *core.Tasks
	tickets *core.Tickets
	engine  *pipeline.Engine
}

func newEngine(t *testing.T, global config.Defaults, repoFiles files) *engineFixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &engineFixture{store: s, tasks: core.NewTasks(s, core.TaskOptions{}), tickets: core.NewTickets(s, core.TicketOptions{})}
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Defaults: global, Files: repoFiles})
	return f
}

func (f *engineFixture) readyTicket(t *testing.T, ref string) {
	t.Helper()
	if _, err := f.tickets.Create(ctx, "human:alex", ref, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tickets.Fire(ctx, "system", ref, "accept"); err != nil {
		t.Fatal(err)
	}
}

func (f *engineFixture) reconcile(t *testing.T) {
	t.Helper()
	all, err := f.tickets.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Reconcile(ctx, all); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (f *engineFixture) state(t *testing.T, ref string) core.TicketState {
	t.Helper()
	tk, err := f.tickets.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	return tk.State
}

// stage returns the ticket's task of a stage in its latest round.
func (f *engineFixture) stage(t *testing.T, ref, stage string) core.Task {
	t.Helper()
	all, _ := f.tasks.List(ctx)
	var found core.Task
	for _, task := range all {
		if task.Ticket == ref && pipeline.StageOf(task) == stage && pipeline.RoundOf(task) >= pipeline.RoundOf(found) {
			found = task
		}
	}
	if found.ID == "" {
		t.Fatalf("%s has no %s task", ref, stage)
	}
	return found
}

func (f *engineFixture) finish(t *testing.T, ref, stage, actor string) {
	t.Helper()
	lease, err := f.tasks.Claim(ctx, actor, f.stage(t, ref, stage).ID)
	if err != nil {
		t.Fatalf("claim %s: %v", stage, err)
	}
	if err := f.tasks.Complete(ctx, lease); err != nil {
		t.Fatal(err)
	}
}

const ref = "gh:acme/web#1"

func TestAReadyTicketStartsAndGetsTheTasksOfItsPipeline(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.readyTicket(t, ref)
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketPlanning {
		t.Fatalf("after the reconcile the ticket is %s, want planning (T3)", st)
	}
	if plan := f.stage(t, ref, "plan"); plan.State != core.TaskReady || plan.Project != "web" || plan.Role != "planner" {
		t.Fatalf("plan task = %+v", plan)
	}
	if impl := f.stage(t, ref, "implement"); impl.State != core.TaskWaiting {
		t.Fatalf("implement is %s, want waiting for the plan", impl.State)
	}
	f.reconcile(t) // idempotent
	all, _ := f.tasks.List(ctx)
	if len(all) != 6 {
		t.Fatalf("%d tasks after two reconciles, want 6", len(all))
	}
}

func TestThePlanGateHoldsT4UntilAHumanApproves(t *testing.T) {
	t.Parallel()
	for _, gate := range []bool{false, true} {
		f := newEngine(t, config.Defaults{PlanGate: gate}, nil)
		f.readyTicket(t, ref)
		f.reconcile(t)
		f.finish(t, ref, "plan", "agent:planner-1")
		f.reconcile(t)
		if !gate {
			if st := f.state(t, ref); st != core.TicketInProgress {
				t.Fatalf("without the gate the ticket is %s after the plan, want in_progress", st)
			}
			continue
		}
		if st := f.state(t, ref); st != core.TicketPlanning {
			t.Fatalf("with the gate the ticket is %s before approval, want planning", st)
		}
		if impl := f.stage(t, ref, "implement"); impl.State != core.TaskWaiting {
			t.Fatalf("with the gate the implementer can start: %s", impl.State)
		}
		f.finish(t, ref, pipeline.PlanApproval, "human:alex")
		f.reconcile(t)
		if st := f.state(t, ref); st != core.TicketInProgress {
			t.Fatalf("after approval the ticket is %s, want in_progress", st)
		}
	}
}

func TestTheTicketFollowsItsTasksToMerge(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1")
	if _, err := f.tickets.Fire(ctx, "agent:implementer-1", ref, "pr_ready"); err != nil { // istari pr open
		t.Fatal(err)
	}
	f.finish(t, ref, "verify", "system")
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInReview {
		t.Fatalf("before the review completes the ticket is %s, want in_review", st)
	}
	f.finish(t, ref, "review", "agent:reviewer-1")
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketAwaitingApproval {
		t.Fatalf("after the review the ticket is %s, want awaiting_approval (T8)", st)
	}
	f.finish(t, ref, "approve", "human:alex")
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketAwaitingApproval {
		t.Fatalf("before the merge completes the ticket is %s, want awaiting_approval", st)
	}
	f.finish(t, ref, "merge", "system")
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketDone {
		t.Fatalf("after the merge the ticket is %s, want done (T11)", st)
	}
}

func TestReviewRoundsAreCountedUpToTheLimit(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, files{"acme/web": "[limits]\nmax_review_rounds = 2\n\n[[stage]]\nname = \"plan\"\nrole = \"planner\"\n\n[[stage]]\nname = \"implement\"\nrole = \"implementer\"\nneeds = [\"plan\"]\n\n[[stage]]\nname = \"review\"\nrole = \"reviewer\"\nneeds = [\"implement\", \"verify\"]\n"})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	first := f.stage(t, ref, "implement")
	if err := f.engine.RequestChanges(ctx, ref, "agent:reviewer-1"); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	second := f.stage(t, ref, "implement")
	if second.ID == first.ID || pipeline.RoundOf(second) != 2 {
		t.Fatalf("after changes were requested implement is %s (round %d)", second.ID, pipeline.RoundOf(second))
	}
	if second.State != core.TaskReady {
		t.Fatalf("a rework of an implement that never finished is %s, want ready: it must not wait for the canceled one", second.State)
	}
	if merge := f.stage(t, ref, "merge"); pipeline.RoundOf(merge) != 2 {
		t.Fatalf("merge of round %d, want round 2", pipeline.RoundOf(merge))
	}
	err := f.engine.RequestChanges(ctx, ref, "agent:reviewer-1")
	if err == nil || !strings.Contains(err.Error(), "max_review_rounds") {
		t.Fatalf("a third round: %v, want refused by max_review_rounds", err)
	}
	if tk, _ := f.tickets.Get(ctx, ref); tk.Hold != "attempts_exhausted" {
		t.Fatalf("hold = %q, want attempts_exhausted after the last round", tk.Hold)
	}
}

func TestARepositoryWithoutAnApproveStageGetsNoApproveTask(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, files{"acme/web": "[[stage]]\nname = \"plan\"\nrole = \"planner\"\n\n[[stage]]\nname = \"implement\"\nrole = \"implementer\"\nneeds = [\"plan\"]\n"})
	f.readyTicket(t, ref)
	f.reconcile(t)
	all, _ := f.tasks.List(ctx)
	for _, task := range all {
		if pipeline.StageOf(task) == "approve" || pipeline.StageOf(task) == "review" {
			t.Fatalf("a task of a removed stage: %+v", task)
		}
	}
}

func TestATicketWithoutTheEnginesTasksIsNotMovedOn(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.readyTicket(t, ref)
	for _, tr := range []string{"start", "plan_accepted", "pr_ready"} {
		if _, err := f.tickets.Fire(ctx, "system", ref, tr); err != nil {
			t.Fatal(err)
		}
	}
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInReview {
		t.Fatalf("a ticket in review with no review task went on to %s", st)
	}
	// A ticket in planning with tasks someone made by hand is theirs.
	other := "gh:acme/web#2"
	f.readyTicket(t, other)
	if _, err := f.tickets.Fire(ctx, "system", other, "start"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: other, Project: "web", Role: "planner"}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	all, _ := f.tasks.List(ctx)
	n := 0
	for _, task := range all {
		if task.Ticket == other {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%s has %d tasks, want only the hand-made one", other, n)
	}
}

func TestAnInvalidPipelineDoesNotStartTheTicket(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, files{"acme/web": "[[stage]]\nname = \"x\"\nrole = \"wizard\"\n"})
	f.readyTicket(t, ref)
	all, _ := f.tickets.List(ctx)
	if err := f.engine.Reconcile(ctx, all); err == nil || !strings.Contains(err.Error(), "wizard") {
		t.Fatalf("Reconcile = %v, want the pipeline's error", err)
	}
	if st := f.state(t, ref); st != core.TicketReady {
		t.Fatalf("the ticket is %s, want ready: its pipeline is invalid", st)
	}
}

func TestAStageWithoutNeedsFollowsThePreviousOne(t *testing.T) {
	t.Parallel()
	// The scaffolded file: approve names no needs.
	f := newEngine(t, config.Defaults{}, files{"acme/web": "[[stage]]\nname = \"plan\"\nrole = \"planner\"\n\n[[stage]]\nname = \"implement\"\nrole = \"implementer\"\nneeds = [\"plan\"]\n\n[[stage]]\nname = \"review\"\nrole = \"reviewer\"\nneeds = [\"implement\", \"verify\"]\n\n[[stage]]\nname = \"approve\"\nkind = \"human\"\nvia = \"github_review\"\n"})
	f.readyTicket(t, ref)
	f.reconcile(t)
	if approve := f.stage(t, ref, "approve"); approve.State != core.TaskWaiting || len(approve.Needs) != 1 || pipeline.StageOf(core.Task{ID: approve.Needs[0]}) != "review" {
		t.Fatalf("approve = %+v, want waiting for review", approve)
	}
	if approve := f.stage(t, ref, "approve"); approve.Role == pipeline.RoleHuman {
		t.Fatal("a GitHub review approval is a gate for istari gate approve")
	}
}

func TestATicketStartsOnlyWhenTheTicketsItNeedsAreDone(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	other := "gh:acme/web#2"
	if _, err := f.tickets.Create(ctx, "human:alex", other, "web"); err != nil {
		t.Fatal(err)
	}
	f.readyTicket(t, ref)
	if _, err := f.tickets.SetNeeds(ctx, "system", ref, []string{other}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketReady {
		t.Fatalf("with its need open the ticket is %s, want ready", st)
	}
	if _, err := f.tickets.Fire(ctx, "system", other, "cancel"); err != nil { // finished, not done
		t.Fatal(err)
	}
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.State != core.TicketReady || tk.Hold != "waiting_dependency" {
		t.Fatalf("with its need canceled the ticket is %s, hold %q; want ready and held", tk.State, tk.Hold)
	}
}

func TestATicketWhoseNeedsAreDoneStarts(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	done := "gh:acme/web#3"
	if _, err := f.tickets.Create(ctx, "human:alex", done, "web"); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []string{"accept", "start", "plan_accepted", "pr_ready", "review_passed", "merged"} {
		if _, err := f.tickets.Fire(ctx, "system", done, tr); err != nil {
			t.Fatal(err)
		}
	}
	f.readyTicket(t, ref)
	if _, err := f.tickets.SetNeeds(ctx, "system", ref, []string{done}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketPlanning {
		t.Fatalf("with its need done the ticket is %s, want planning", st)
	}
}

func TestATicketWaitingForAnUnfinishedNeedIsHeldAndFreedWhenItIsDone(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	need := "gh:acme/web#2"
	if _, err := f.tickets.Create(ctx, "human:alex", need, "web"); err != nil {
		t.Fatal(err)
	}
	f.readyTicket(t, ref)
	if _, err := f.tickets.SetNeeds(ctx, "system", ref, []string{need}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.Hold != "waiting_dependency" || tk.State != core.TicketReady {
		t.Fatalf("the waiting ticket = %s, hold %q; want ready on waiting_dependency", tk.State, tk.Hold)
	}
	for _, tr := range []string{"accept", "start", "plan_accepted", "pr_ready", "review_passed", "merged"} {
		f.tickets.Fire(ctx, "system", need, tr)
	}
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.Hold != "" || tk.State != core.TicketPlanning {
		t.Fatalf("after its need is done the ticket = %s, hold %q; want planning, no hold", tk.State, tk.Hold)
	}
}

// gainsAnOpenNeed takes ref to the given state, then gives it a need on
// a new, open ticket after that state was reached. It returns the need.
func (f *engineFixture) gainsAnOpenNeed(t *testing.T, state core.TicketState) string {
	t.Helper()
	need := "gh:acme/web#2"
	if _, err := f.tickets.Create(ctx, "human:alex", need, "web"); err != nil {
		t.Fatal(err)
	}
	f.readyTicket(t, ref)
	f.reconcile(t)
	if state != core.TicketPlanning {
		f.finish(t, ref, "plan", "agent:planner-1")
		f.reconcile(t)
	}
	if state == core.TicketInReview {
		f.finish(t, ref, "implement", "agent:implementer-1")
		if _, err := f.tickets.Fire(ctx, "agent:implementer-1", ref, "pr_ready"); err != nil { // istari pr open
			t.Fatal(err)
		}
	}
	if st := f.state(t, ref); st != state {
		t.Fatalf("the ticket is %s before it gains a need, want %s", st, state)
	}
	if _, err := f.tickets.SetNeeds(ctx, "agent:refiner-1", ref, []string{need}); err != nil { // the refiner's rewrite
		t.Fatal(err)
	}
	return need
}

func TestAPlanningTicketWhoseNeedsGainAnOpenTicketIsHeld(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.gainsAnOpenNeed(t, core.TicketPlanning)
	f.reconcile(t)
	if st, hold := f.state(t, ref), f.hold(t, ref); st != core.TicketPlanning || hold != "waiting_dependency" {
		t.Fatalf("a planning ticket with an open need = %s, hold %q; want planning on waiting_dependency", st, hold)
	}
	if l, err := f.tasks.ClaimReady(ctx, "agent:planner-1", "planner"); err == nil {
		t.Fatalf("the held ticket's plan task was handed out: %+v", l)
	}
}

func TestAnInProgressTicketWhoseNeedsGainAnOpenTicketIsHeld(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.gainsAnOpenNeed(t, core.TicketInProgress)
	f.reconcile(t)
	if st, hold := f.state(t, ref), f.hold(t, ref); st != core.TicketInProgress || hold != "waiting_dependency" {
		t.Fatalf("an in_progress ticket with an open need = %s, hold %q; want in_progress on waiting_dependency", st, hold)
	}
	if l, err := f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer"); err == nil {
		t.Fatalf("the held ticket's implement task was handed out: %+v", l)
	}
}

func TestAHeldInProgressTicketGoesOnInItsRoundWhenItsNeedIsDone(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	need := f.gainsAnOpenNeed(t, core.TicketInProgress)
	f.reconcile(t)
	for _, tr := range []string{"accept", "start", "plan_accepted", "pr_ready", "review_passed", "merged"} {
		if _, err := f.tickets.Fire(ctx, "system", need, tr); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.tasks.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if st, hold := f.state(t, ref), f.hold(t, ref); st != core.TicketInProgress || hold != "" {
		t.Fatalf("after its need is done the ticket = %s, hold %q; want in_progress, no hold", st, hold)
	}
	after, err := f.tasks.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("tasks went from %d to %d; want the ticket to go on with its round", len(before), len(after))
	}
	l, err := f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer")
	if err != nil {
		t.Fatalf("the implement task is not handed out after the hold: %v", err)
	}
	if got := pipeline.RoundOf(core.Task{ID: l.Task}); got != 1 {
		t.Fatalf("the handed-out implement task is of round %d, want 1", got)
	}
}

func TestAnInReviewTicketThatGainsAnOpenNeedIsNotHeldAndNotedOnce(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	j := journal.New(nil, f.store, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Journal: j})
	f.gainsAnOpenNeed(t, core.TicketInReview)
	f.reconcile(t)
	f.reconcile(t)
	if st, hold := f.state(t, ref), f.hold(t, ref); st != core.TicketInReview || hold != "" {
		t.Fatalf("an in_review ticket with an open need = %s, hold %q; want in_review, no hold", st, hold)
	}
	r, err := tickets.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := j.Stored(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, e := range entries {
		if e.Kind == journal.Note && strings.Contains(e.Text, "after its pull request was opened") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("journal = %+v; want one note that the ticket got open needs after its pull request was opened", entries)
	}
}

func TestANewTicketBelowTheStandardGetsOneRefineTask(t *testing.T) {
	t.Parallel()
	s := func(ctx context.Context, tk core.Ticket) (bool, error) { return tk.Ref == ref, nil }
	f := newEngine(t, config.Defaults{}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{NeedsRefine: s})
	if _, err := f.tickets.Create(ctx, "human:bob", ref, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tickets.Create(ctx, "human:bob", "gh:acme/web#2", "web"); err != nil { // meets it
		t.Fatal(err)
	}
	f.reconcile(t)
	f.reconcile(t)
	all, _ := f.tasks.List(ctx)
	if len(all) != 1 || all[0].Ticket != ref || all[0].Role != "refiner" || all[0].State != core.TaskReady || pipeline.StageOf(all[0]) != "refine-1" {
		t.Fatalf("tasks = %+v, want one ready refiner task for %s", all, ref)
	}
}

func TestRefiningIsRepeatedAfterAnAnswerUpToThreeTimes(t *testing.T) {
	t.Parallel()
	s := func(ctx context.Context, tk core.Ticket) (bool, error) { return true, nil }
	f := newEngine(t, config.Defaults{}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{NeedsRefine: s})
	if _, err := f.tickets.Create(ctx, "human:bob", ref, "web"); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		f.reconcile(t)
		refine := f.stage(t, ref, fmt.Sprintf("refine-%d", attempt))
		if refine.State != core.TaskReady {
			t.Fatalf("attempt %d: %+v", attempt, refine)
		}
		f.finish(t, ref, refine.ID[strings.LastIndex(refine.ID, "_")+1:], "agent:refiner-1")
	}
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.Hold != "attempts_exhausted" {
		t.Fatalf("after three refines the ticket's hold is %q, want attempts_exhausted", tk.Hold)
	}
}

func TestATicketAboveMaxSizeDoesNotReachT4WithoutASplit(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, files{"acme/web": "[limits]\nmax_size = \"M\"\n"})
	f.readyTicket(t, ref)
	if _, err := f.tickets.SetSize(ctx, "agent:refiner-1", ref, "L"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketPlanning {
		t.Fatalf("an L ticket with max_size M is %s after its plan, want planning", st)
	}
}

func TestASplitParentIsDoneWhenAllItsChildrenAre(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.readyTicket(t, ref)
	f.reconcile(t)
	kids := []string{"gh:acme/web#2", "gh:acme/web#3"}
	for _, k := range kids {
		if _, err := f.tickets.Create(ctx, "human:alex", k, "web"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.tickets.SetChildren(ctx, "agent:planner-1", ref, kids); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tickets.Fire(ctx, "agent:planner-1", ref, "split"); err != nil { // T5
		t.Fatal(err)
	}
	done := func(k string) {
		for _, tr := range []string{"accept", "start", "plan_accepted", "pr_ready", "review_passed", "merged"} {
			f.tickets.Fire(ctx, "system", k, tr)
		}
	}
	done(kids[0])
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInProgress {
		t.Fatalf("with one child open the parent is %s", st)
	}
	done(kids[1])
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketDone {
		t.Fatalf("with all children done the parent is %s, want done (T12)", st)
	}
}

func TestAPlanTaskDoneWithoutAPlanOrTooLargeHoldsTheTicket(t *testing.T) {
	t.Parallel()
	noPlan := func(context.Context, string) (bool, error) { return false, nil }
	f := newEngine(t, config.Defaults{}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{HasPlan: noPlan})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.State != core.TicketPlanning || tk.Hold != "needs_input" {
		t.Fatalf("a plan task done without a plan: %s, hold %q; want planning on needs_input", tk.State, tk.Hold)
	}
}

func TestACanceledChildHoldsItsParent(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.readyTicket(t, ref)
	f.reconcile(t)
	kid := "gh:acme/web#2"
	f.tickets.Create(ctx, "human:alex", kid, "web")
	f.tickets.SetChildren(ctx, "agent:planner-1", ref, []string{kid})
	if _, err := f.tickets.Fire(ctx, "agent:planner-1", ref, "split"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tickets.Fire(ctx, "human:alex", kid, "cancel"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.Hold != "waiting_dependency" {
		t.Fatalf("a parent with a canceled child has hold %q, want waiting_dependency", tk.Hold)
	}
	cleared, err := f.store.Read(ctx, "", store.Filter{Ticket: ref, TypePrefix: core.EventTicketHoldCleared}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 0 {
		t.Fatalf("the canceled child's hold was cleared %d times; a human clears it", len(cleared))
	}
}

func TestT6FiresOnlyWhenCIIsGreenAndRedCIMeansRework(t *testing.T) {
	t.Parallel()
	ci := map[string]string{}
	checks := func(_ context.Context, ref string) (string, bool) { s, ok := ci[ref]; return s, ok }
	f := newEngine(t, config.Defaults{}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Checks: checks})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	original := f.stage(t, ref, "implement")
	f.finish(t, ref, "implement", "agent:implementer-1") // it opened the pull request
	ci[ref] = "pending"
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInProgress {
		t.Fatalf("with CI pending the ticket is %s, want in_progress", st)
	}
	ci[ref] = "failure"
	f.reconcile(t)
	if impl := f.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 2 || impl.State != core.TaskReady {
		t.Fatalf("after red CI implement is round %d %s, want a ready rework in round 2", pipeline.RoundOf(impl), impl.State)
	} else if len(impl.Needs) != 1 || impl.Needs[0] != original.ID {
		t.Fatalf("the rework needs %v, want the original implement %s (spec 4.3)", impl.Needs, original.ID)
	}
	f.reconcile(t) // the red commit is handled: round 2 is not done yet
	if impl := f.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 2 {
		t.Fatalf("one red commit started round %d", pipeline.RoundOf(impl))
	}
	f.finish(t, ref, "implement", "agent:implementer-1")
	ci[ref] = "success"
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInReview {
		t.Fatalf("with CI green the ticket is %s, want in_review (T6)", st)
	}
	if v := f.stage(t, ref, "verify"); v.State != core.TaskCompleted {
		t.Fatalf("verify is %s, want completed by green CI", v.State)
	}
}

func TestAPullRequestWithoutCIHoldsTheTicketUntilChecksAreGreen(t *testing.T) {
	t.Parallel()
	ci := "none"
	checks := func(context.Context, string) (string, bool) { return ci, true }
	f := newEngine(t, config.Defaults{}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Checks: checks})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1")
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.State != core.TicketInProgress || tk.Hold != "needs_input" {
		t.Fatalf("a pull request without CI: %s, hold %q; want in_progress on needs_input", tk.State, tk.Hold)
	}
	if v := f.stage(t, ref, "verify"); v.State != core.TaskReady {
		t.Fatalf("verify is %s, want still ready", v.State)
	}
	ci = "success" // a human added CI
	if _, err := f.tickets.ClearHold(ctx, "human:alex", ref, "needs_input"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInReview {
		t.Fatalf("with CI green and the hold cleared the ticket is %s, want in_review (T6)", st)
	}
}

func TestOnGreenCIThePullRequestIsMarkedReadyBeforeT6(t *testing.T) {
	t.Parallel()
	ci := "pending"
	checks := func(context.Context, string) (string, bool) { return ci, true }
	var f *engineFixture
	var readied []string
	var stateWhenReadied core.TicketState
	ready := func(_ context.Context, r string) error {
		readied = append(readied, r)
		stateWhenReadied = f.state(t, r)
		return nil
	}
	f = newEngine(t, config.Defaults{}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Checks: checks, Ready: ready})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1")
	f.reconcile(t)
	if len(readied) != 0 {
		t.Fatalf("with CI pending Ready was called for %v", readied)
	}
	ci = "success"
	f.reconcile(t)
	if len(readied) != 1 || readied[0] != ref {
		t.Fatalf("Ready was called for %v, want once for %s", readied, ref)
	}
	if stateWhenReadied != core.TicketInProgress {
		t.Fatalf("Ready saw the ticket %s, want in_progress: the draft ends before T6", stateWhenReadied)
	}
	if st := f.state(t, ref); st != core.TicketInReview {
		t.Fatalf("after Ready the ticket is %s, want in_review (T6)", st)
	}
}

func TestWhenMarkingReadyFailsT6WaitsForTheNextReconcile(t *testing.T) {
	t.Parallel()
	checks := func(context.Context, string) (string, bool) { return "success", true }
	readyErr := errors.New("github: 502")
	ready := func(context.Context, string) error { return readyErr }
	f := newEngine(t, config.Defaults{}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Checks: checks, Ready: ready})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1")
	all, err := f.tickets.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Reconcile(ctx, all); !errors.Is(err, readyErr) {
		t.Fatalf("Reconcile = %v, want the failure to mark ready", err)
	}
	if st := f.state(t, ref); st != core.TicketInProgress {
		t.Fatalf("with the pull request still a draft the ticket is %s, want in_progress", st)
	}
	if v := f.stage(t, ref, "verify"); v.State != core.TaskReady {
		t.Fatalf("verify is %s, want still ready", v.State)
	}
	readyErr = nil
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInReview {
		t.Fatalf("once marked ready the ticket is %s, want in_review (T6)", st)
	}
}

func TestAReworkThatLeavesCIRedCountsAsARound(t *testing.T) {
	t.Parallel()
	checks := func(context.Context, string) (string, bool) { return "failure", true }
	f := newEngine(t, config.Defaults{MaxReviewRounds: 2}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Defaults: config.Defaults{MaxReviewRounds: 2}, Checks: checks})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1")
	f.reconcile(t) // red: round 2
	f.finish(t, ref, "implement", "agent:implementer-1")
	f.reconcile(t) // still red after round 2: the rounds are used up
	tk, err := f.tickets.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Hold != "attempts_exhausted" {
		t.Fatalf("after two rounds with CI red the ticket is %s, hold %q", tk.State, tk.Hold)
	}
}

func TestARedCIThatIsRerunStartsNoRoundAndAGreenRerunGoesToReview(t *testing.T) {
	t.Parallel()
	ci := "failure"
	checks := func(context.Context, string) (string, bool) { return ci, ci != "" }
	var reruns int
	rerun := func(context.Context, string) (pipeline.RerunOutcome, error) {
		reruns++
		ci = "" // the watch forgets the red result until it reads the rerun's
		if reruns == 1 {
			return pipeline.RerunStarted, nil
		}
		return pipeline.RerunNone, nil
	}
	defaults := config.Defaults{MaxReviewRounds: 1}
	f := newEngine(t, defaults, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Defaults: defaults, Checks: checks, Rerun: rerun})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1") // it answered the red CI as unrelated
	f.reconcile(t)
	if impl := f.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 1 {
		t.Fatalf("a rerun started round %d, want none", pipeline.RoundOf(impl))
	}
	if v := f.stage(t, ref, "verify"); v.State != core.TaskReady {
		t.Fatalf("verify is %s, want still ready: it waits for the rerun", v.State)
	}
	ci = "success"
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.State != core.TicketInReview || tk.Hold != "" {
		t.Fatalf("after a green rerun with max_review_rounds 1 the ticket is %s, hold %q; want in_review (T6)", tk.State, tk.Hold)
	}
}

func TestARedCINotRerunStartsTheReworkRound(t *testing.T) {
	t.Parallel()
	checks := func(context.Context, string) (string, bool) { return "failure", true }
	rerun := func(context.Context, string) (pipeline.RerunOutcome, error) { return pipeline.RerunNone, nil } // not answered
	f := newEngine(t, config.Defaults{}, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Checks: checks, Rerun: rerun})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1")
	f.reconcile(t)
	if impl := f.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 2 || impl.State != core.TaskReady {
		t.Fatalf("implement is round %d %s, want a ready rework in round 2", pipeline.RoundOf(impl), impl.State)
	}
}

// watchedPR is a ticket whose pull request the real watch follows over the
// fake GitHub, its head abc123 with the checks given, and an engine wired to
// the watch as the daemon wires it.
type watchedPR struct {
	*engineFixture
	gh      *githubtest.Server
	watch   *vcs.Watcher
	journal *journal.Journal
}

func newWatchedPR(t *testing.T, defaults config.Defaults, checks ...githubtest.Check) *watchedPR {
	t.Helper()
	f := newEngine(t, defaults, nil)
	gh := githubtest.New(t)
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	j := journal.New(nil, f.store, nil)
	w := vcs.NewWatcher(c, f.store, vcs.WatcherOptions{Journal: j})
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: ref, Head: "istari/1/x", Base: "main", Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Track(ctx, ref, githubtest.Repo, pr); err != nil {
		t.Fatal(err)
	}
	gh.SetHeadSHA(pr.Number, "abc123")
	gh.SetChecks("abc123", checks...)
	state := func(ctx context.Context, ref string) (string, bool) {
		pr, err := w.PullRequestOf(ctx, ref)
		return pr.Checks, err == nil && pr.Checks != ""
	}
	rerun := func(ctx context.Context, ref string) (pipeline.RerunOutcome, error) {
		o, err := w.Rerun(ctx, ref)
		return map[vcs.RerunOutcome]pipeline.RerunOutcome{vcs.RerunNone: pipeline.RerunNone, vcs.RerunStarted: pipeline.RerunStarted, vcs.RerunSpent: pipeline.RerunSpent}[o], err
	}
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Defaults: defaults, Checks: state, Rerun: rerun})
	return &watchedPR{engineFixture: f, gh: gh, watch: w, journal: j}
}

// answeredUnrelated takes the ticket to its first implementer, which sees
// the red CI, answers it as not this change and completes.
func (p *watchedPR) answeredUnrelated(t *testing.T) {
	t.Helper()
	p.readyTicket(t, ref)
	p.reconcile(t)
	p.finish(t, ref, "plan", "agent:planner-1")
	p.reconcile(t)
	if err := p.watch.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.watch.Unrelated(ctx, ref, "the known ETXTBSY flake"); err != nil {
		t.Fatalf("Unrelated: %v", err)
	}
	p.finish(t, ref, "implement", "agent:implementer-1")
	p.reconcile(t)
	if err := p.watch.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	p.reconcile(t)
}

func TestACheckThatFailsOnceAndPassesOnRerunGoesToReviewWithoutASecondRound(t *testing.T) {
	t.Parallel()
	p := newWatchedPR(t, config.Defaults{MaxReviewRounds: 1}, githubtest.Check{Name: "test", Status: "completed", Conclusion: "failure", Rerun: "success"})
	p.answeredUnrelated(t)
	if p.gh.Reruns() != 1 {
		t.Fatalf("the fake saw %d reruns, want 1", p.gh.Reruns())
	}
	if impl := p.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 1 {
		t.Fatalf("the rerun started round %d, want none", pipeline.RoundOf(impl))
	}
	if tk, _ := p.tickets.Get(ctx, ref); tk.State != core.TicketInReview || tk.Hold != "" {
		t.Fatalf("the ticket is %s, hold %q; want in_review (T6)", tk.State, tk.Hold)
	}
}

func TestAFailedJobIsRerunOnlyOnceItsRunHasFinished(t *testing.T) {
	t.Parallel()
	p := newWatchedPR(t, config.Defaults{MaxReviewRounds: 1},
		githubtest.Check{Name: "test", Status: "completed", Conclusion: "failure", Rerun: "success"},
		githubtest.Check{Name: "race", Status: "in_progress"},
	)
	p.answeredUnrelated(t)
	if p.gh.Reruns() != 0 {
		t.Fatalf("the fake saw %d reruns while race runs, want none", p.gh.Reruns())
	}
	if impl := p.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 1 {
		t.Fatalf("a pending head started round %d, want none", pipeline.RoundOf(impl))
	}
	p.gh.SetCheck("abc123", "race", "completed", "success")
	for range 2 { // the first reads the head red and reruns, the second reads the rerun
		if err := p.watch.Poll(ctx); err != nil {
			t.Fatal(err)
		}
		p.reconcile(t)
	}
	if p.gh.Reruns() != 1 {
		t.Fatalf("the fake saw %d reruns once the run finished, want 1", p.gh.Reruns())
	}
	if impl := p.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 1 {
		t.Fatalf("the rerun started round %d, want none", pipeline.RoundOf(impl))
	}
	if tk, _ := p.tickets.Get(ctx, ref); tk.State != core.TicketInReview || tk.Hold != "" {
		t.Fatalf("the ticket is %s, hold %q; want in_review (T6)", tk.State, tk.Hold)
	}
}

func TestACheckThatFailsOnRerunTooStartsTheNextRoundAndTheJournalNamesBoth(t *testing.T) {
	t.Parallel()
	p := newWatchedPR(t, config.Defaults{}, githubtest.Check{Name: "test", Status: "completed", Conclusion: "failure", Rerun: "failure"})
	p.answeredUnrelated(t)
	if impl := p.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 2 || impl.State != core.TaskReady {
		t.Fatalf("implement is round %d %s, want a ready rework in round 2", pipeline.RoundOf(impl), impl.State)
	}
	r, err := tickets.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := p.journal.Stored(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var first, rerun bool
	for _, e := range entries {
		if e.Kind == journal.CI && e.CI.Commit == "abc123" {
			first, rerun = first || !e.CI.Rerun, rerun || e.CI.Rerun
		}
	}
	if !first || !rerun {
		t.Fatalf("journal = %+v, want a ci entry of the first failure and one of the rerun", entries)
	}
}

func TestARecoveredTicketWithoutTasksGetsTheStagesOfItsState(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		fires       []string
		first, none string // the first stage it gets, and one before it that it must not get
	}{
		{[]string{"accept", "start", "plan_accepted"}, "implement", "plan"},
		{[]string{"accept", "start", "plan_accepted", "pr_ready"}, "review", "implement"},
		{[]string{"accept", "start", "plan_accepted", "pr_ready", "review_passed"}, "approve", "review"},
	} {
		f := newEngine(t, config.Defaults{}, nil)
		if _, err := f.tickets.Create(ctx, "human:alex", ref, "web"); err != nil {
			t.Fatal(err)
		}
		for _, tr := range c.fires {
			if _, err := f.tickets.Fire(ctx, "system", ref, tr); err != nil {
				t.Fatal(err)
			}
		}
		state := f.state(t, ref)
		f.reconcile(t)
		if task := f.stage(t, ref, c.first); task.State != core.TaskReady {
			t.Errorf("%s: %s is %s, want ready", state, c.first, task.State)
		}
		all, _ := f.tasks.List(ctx)
		for _, task := range all {
			if pipeline.StageOf(task) == c.none {
				t.Errorf("%s: it got a %s task", state, c.none)
			}
		}
	}
}

// reports are the findings code accepted in the report of each review task.
type reports map[string]int

func (r reports) of(_ context.Context, _, task string) (int, bool, error) {
	n, ok := r[task]
	return n, ok, nil
}

// inReview takes the ticket to in_review with the given CI and reports.
func inReview(t *testing.T, defaults config.Defaults, ci map[string]string, r reports) *engineFixture {
	t.Helper()
	checks := func(_ context.Context, ref string) (string, bool) { s, ok := ci[ref]; return s, ok }
	f := newEngine(t, defaults, nil)
	f.engine = pipeline.NewEngine(f.tasks, f.tickets, pipeline.EngineOptions{Defaults: defaults, Checks: checks, Review: r.of})
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	f.finish(t, ref, "implement", "agent:implementer-1")
	ci[ref] = "success"
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInReview {
		t.Fatalf("the ticket is %s, want in_review", st)
	}
	return f
}

func TestOneAcceptedFindingReturnsTheTicketForRework(t *testing.T) {
	t.Parallel()
	r := reports{}
	f := inReview(t, config.Defaults{}, map[string]string{}, r)
	review := f.stage(t, ref, "review")
	r[review.ID] = 1
	f.finish(t, ref, "review", "agent:reviewer-1")
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInProgress {
		t.Fatalf("with one accepted finding the ticket is %s, want in_progress (T7)", st)
	}
	if impl := f.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 2 || impl.State != core.TaskReady {
		t.Fatalf("implement is round %d %s, want a ready rework in round 2", pipeline.RoundOf(impl), impl.State)
	}
}

func TestNoAcceptedFindingsAndGreenCIMoveTheTicketToAwaitingApproval(t *testing.T) {
	t.Parallel()
	r, ci := reports{}, map[string]string{}
	f := inReview(t, config.Defaults{}, ci, r)
	r[f.stage(t, ref, "review").ID] = 0
	f.finish(t, ref, "review", "agent:reviewer-1")
	ci[ref] = "pending" // a new push after the review
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketInReview {
		t.Fatalf("with CI pending the ticket is %s, want in_review until it is green", st)
	}
	ci[ref] = "success"
	f.reconcile(t)
	if st := f.state(t, ref); st != core.TicketAwaitingApproval {
		t.Fatalf("with no findings and CI green the ticket is %s, want awaiting_approval (T8)", st)
	}
}

func TestFindingsAfterTheLastReviewRoundHoldTheTicket(t *testing.T) {
	t.Parallel()
	r, ci := reports{}, map[string]string{}
	f := inReview(t, config.Defaults{MaxReviewRounds: 1}, ci, r)
	r[f.stage(t, ref, "review").ID] = 2
	f.finish(t, ref, "review", "agent:reviewer-1")
	f.reconcile(t)
	tk, err := f.tickets.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != core.TicketInReview || tk.Hold != "attempts_exhausted" {
		t.Fatalf("after the last round the ticket is %s, hold %q; want in_review on hold attempts_exhausted", tk.State, tk.Hold)
	}
}

func TestAReviewDoneWithoutAReportGivesNoVerdict(t *testing.T) {
	t.Parallel()
	f := inReview(t, config.Defaults{}, map[string]string{}, reports{})
	f.finish(t, ref, "review", "human:alex") // only a human can: a reviewer's complete needs a report
	f.reconcile(t)
	tk, err := f.tickets.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != core.TicketInReview || tk.Hold != "" {
		t.Fatalf("a review without a report leaves the ticket %s, hold %q; want in_review, no verdict and no hold", tk.State, tk.Hold)
	}
}

// conflictFixture is an engine whose PR watch reports the pull requests in
// dirty (ticket → pull request and head) in conflict with main, with its
// ticket in_review.
type conflictFixture struct {
	*engineFixture
	dirty   map[string]pipeline.PRConflict
	ci      map[string]string
	reports reports
	journal *journal.Journal
}

func inReviewWithConflicts(t *testing.T, defaults config.Defaults) *conflictFixture {
	t.Helper()
	c := &conflictFixture{dirty: map[string]pipeline.PRConflict{}, ci: map[string]string{}, reports: reports{}}
	c.engineFixture = newEngine(t, defaults, nil)
	c.journal = journal.New(nil, c.store, nil)
	checks := func(_ context.Context, ref string) (string, bool) { s, ok := c.ci[ref]; return s, ok }
	conflict := func(_ context.Context, ref string) (pipeline.PRConflict, bool, error) {
		pr, ok := c.dirty[ref]
		return pr, ok, nil
	}
	c.engine = pipeline.NewEngine(c.tasks, c.tickets, pipeline.EngineOptions{Defaults: defaults, Checks: checks, Review: c.reports.of, Journal: c.journal, Conflict: conflict})
	c.readyTicket(t, ref)
	c.reconcile(t)
	c.finish(t, ref, "plan", "agent:planner-1")
	c.reconcile(t)
	c.finish(t, ref, "implement", "agent:implementer-1")
	c.ci[ref] = "success"
	c.reconcile(t)
	if st := c.state(t, ref); st != core.TicketInReview {
		t.Fatalf("the ticket is %s, want in_review", st)
	}
	return c
}

// review completes the latest review with findings and reconciles.
func (c *conflictFixture) review(t *testing.T, findings int) {
	t.Helper()
	c.reports[c.stage(t, ref, "review").ID] = findings
	c.finish(t, ref, "review", "agent:reviewer-1")
	c.reconcile(t)
}

// lastTrigger returns the trigger of the ticket's latest transition.
func (c *conflictFixture) lastTrigger(t *testing.T) string {
	t.Helper()
	events, err := c.store.Read(ctx, "", store.Filter{Ticket: ref, TypePrefix: "ticket.transitioned"}, 0)
	if err != nil || len(events) == 0 {
		t.Fatalf("transitions: %v, %v", events, err)
	}
	var p struct{ Trigger string }
	if err := json.Unmarshal(events[len(events)-1].Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p.Trigger
}

// notesNaming returns the ticket's journal notes that contain text.
func (c *conflictFixture) notesNaming(t *testing.T, text string) []string {
	t.Helper()
	r, err := tickets.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.journal.Stored(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range entries {
		if en.Kind == journal.Note && strings.Contains(en.Text, text) {
			out = append(out, en.Text)
		}
	}
	return out
}

func (c *conflictFixture) taskCount(t *testing.T) int {
	t.Helper()
	all, err := c.tasks.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// assertConflictRound checks that the ticket returned by merge_failed with
// a ready implement in round 2 after the completed one, a journal note that
// names the pull request and the head once, and that reconciling again changes nothing.
func (c *conflictFixture) assertConflictRound(t *testing.T, head string) {
	t.Helper()
	if st, tr := c.state(t, ref), c.lastTrigger(t); st != core.TicketInProgress || tr != "merge_failed" {
		t.Fatalf("with a conflict the ticket is %s by %s, want in_progress by merge_failed (T10)", st, tr)
	}
	impl := c.stage(t, ref, "implement")
	if pipeline.RoundOf(impl) != 2 || impl.State != core.TaskReady || len(impl.Needs) != 1 || pipeline.RoundOf(core.Task{ID: impl.Needs[0]}) != 1 {
		t.Fatalf("implement = %+v (round %d), want a ready round 2 after round 1's", impl, pipeline.RoundOf(impl))
	}
	if notes := c.notesNaming(t, head); len(notes) != 1 || !strings.Contains(notes[0], "pull request #7") {
		t.Fatalf("notes naming %s = %q, want one that names pull request #7", head, notes)
	}
	tasks := c.taskCount(t)
	c.reconcile(t)
	if st, n, notes := c.state(t, ref), c.taskCount(t), c.notesNaming(t, head); st != core.TicketInProgress || n != tasks || len(notes) != 1 {
		t.Fatalf("a second reconcile: %s, %d tasks (was %d), %d notes; want nothing new", st, n, tasks, len(notes))
	}
}

func TestAPullRequestInConflictReturnsAnAwaitingApprovalTicketForRework(t *testing.T) {
	t.Parallel()
	c := inReviewWithConflicts(t, config.Defaults{})
	c.review(t, 0)
	if st := c.state(t, ref); st != core.TicketAwaitingApproval {
		t.Fatalf("the ticket is %s without a conflict, want awaiting_approval", st)
	}
	approve, merge := c.stage(t, ref, "approve"), c.stage(t, ref, "merge")
	c.dirty[ref] = pipeline.PRConflict{PR: 7, Head: "h1"}
	c.reconcile(t)
	for _, id := range []string{approve.ID, merge.ID} {
		if task, _ := c.tasks.Get(ctx, id); task.State != core.TaskCanceled {
			t.Fatalf("%s is %s, want canceled", id, task.State)
		}
	}
	c.assertConflictRound(t, "h1")
}

func TestAPullRequestInConflictReturnsAnInReviewTicketAndEndsItsReview(t *testing.T) {
	t.Parallel()
	c := inReviewWithConflicts(t, config.Defaults{})
	review := c.stage(t, ref, "review")
	c.dirty[ref] = pipeline.PRConflict{PR: 7, Head: "h1"}
	c.reconcile(t)
	if task, _ := c.tasks.Get(ctx, review.ID); task.State != core.TaskCanceled {
		t.Fatalf("the open review is %s, want canceled", task.State)
	}
	c.assertConflictRound(t, "h1")
}

func TestAConflictRoundDoesNotCountAgainstMaxReviewRounds(t *testing.T) {
	t.Parallel()
	c := inReviewWithConflicts(t, config.Defaults{MaxReviewRounds: 2})
	c.dirty[ref] = pipeline.PRConflict{PR: 7, Head: "h1"}
	c.reconcile(t) // round 2, a conflict round
	delete(c.dirty, ref)
	c.finish(t, ref, "implement", "agent:implementer-1") // the conflict is resolved and pushed
	c.reconcile(t)
	if st := c.state(t, ref); st != core.TicketInReview {
		t.Fatalf("after the conflict round the ticket is %s, want in_review", st)
	}
	c.review(t, 1)
	if st, hold := c.state(t, ref), c.hold(t, ref); st != core.TicketInProgress || hold != "" {
		t.Fatalf("findings in the first review round after a conflict round: %s, hold %q; want in_progress for rework", st, hold)
	}
	if impl := c.stage(t, ref, "implement"); pipeline.RoundOf(impl) != 3 {
		t.Fatalf("the rework is round %d, want 3", pipeline.RoundOf(impl))
	}
}

// fail claims the ticket's task of a stage and fails it n times.
func (f *engineFixture) fail(t *testing.T, ref, stage string, n int) {
	t.Helper()
	for range n {
		lease, err := f.tasks.Claim(ctx, "agent:planner-1", f.stage(t, ref, stage).ID)
		if err != nil {
			t.Fatalf("claim %s: %v", stage, err)
		}
		if err := f.tasks.Fail(ctx, lease, "boom"); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *engineFixture) hold(t *testing.T, ref string) string {
	t.Helper()
	tk, err := f.tickets.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Hold
}

func TestATaskThatUsesUpItsAttemptsHoldsTheTicket(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.fail(t, ref, "plan", core.DefaultMaxAttempts)
	f.reconcile(t)
	if hold := f.hold(t, ref); hold != "attempts_exhausted" {
		t.Fatalf("hold = %q after the last failed attempt, want attempts_exhausted", hold)
	}
	f.reconcile(t) // idempotent: a held ticket is not held again
	if hold := f.hold(t, ref); hold != "attempts_exhausted" {
		t.Fatalf("hold = %q after a second reconcile, want attempts_exhausted", hold)
	}
}

func TestAFinishedTicketWithAnExhaustedTaskIsNotHeld(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.fail(t, ref, "plan", core.DefaultMaxAttempts)
	if _, err := f.tickets.Fire(ctx, "human:alex", ref, "cancel"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t) // fails if the engine tries to hold a canceled ticket
	if hold := f.hold(t, ref); hold != "" {
		t.Fatalf("hold = %q on a canceled ticket, want none", hold)
	}
}

func TestATaskFailedBelowItsLimitDoesNotHoldTheTicket(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.fail(t, ref, "plan", core.DefaultMaxAttempts-1)
	f.reconcile(t)
	if hold := f.hold(t, ref); hold != "" {
		t.Fatalf("hold = %q after a failed attempt below the limit, want none", hold)
	}
	if plan := f.stage(t, ref, "plan"); plan.State != core.TaskReady {
		t.Fatalf("plan is %s, want ready for its next attempt", plan.State)
	}
}

// Not parallel: it reads the log, which every test of the package shares.
func TestTheEngineLogsAPipelineWarningOncePerRepository(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	early := "[[stage]]\nname = \"implement\"\nrole = \"implementer\"\n\n[[stage]]\nname = \"review\"\nrole = \"reviewer\"\nneeds = [\"implement\"]\n"
	repos := []string{"acme/web", "acme/api"}
	// A cache of a nanosecond: every Load reads the file again.
	e := pipeline.NewEngine(core.NewTasks(s, core.TaskOptions{}), core.NewTickets(s, core.TicketOptions{}),
		pipeline.EngineOptions{Files: files{repos[0]: early, repos[1]: early}, CacheFor: time.Nanosecond})
	for range 3 {
		for _, repo := range repos {
			if _, err := e.Load(ctx, repo); err != nil {
				t.Fatalf("%s: %v", repo, err)
			}
		}
	}
	for _, repo := range repos {
		if n := strings.Count(logged.String(), repo+": .istari/pipeline.toml line 5"); n != 1 {
			t.Errorf("%s: the warning was logged %d times, want once\n%s", repo, n, logged.String())
		}
	}
}
