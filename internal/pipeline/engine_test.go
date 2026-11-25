package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
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
	f := newEngine(t, config.Defaults{}, files{"acme/web": "[[stage]]\nname = \"plan\"\nrole = \"planner\"\n\n[[stage]]\nname = \"implement\"\nrole = \"implementer\"\nneeds = [\"plan\"]\n\n[[stage]]\nname = \"review\"\nrole = \"reviewer\"\nneeds = [\"implement\"]\n\n[[stage]]\nname = \"approve\"\nkind = \"human\"\nvia = \"github_review\"\n"})
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

func TestAPlanningTicketWhoseNeedsGainAnOpenTicketIsHeld(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	need := "gh:acme/web#2"
	if _, err := f.tickets.Create(ctx, "human:alex", need, "web"); err != nil {
		t.Fatal(err)
	}
	f.readyTicket(t, ref)
	f.reconcile(t)
	if _, err := f.tickets.SetNeeds(ctx, "agent:refiner-1", ref, []string{need}); err != nil { // the refiner's rewrite
		t.Fatal(err)
	}
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.State != core.TicketPlanning || tk.Hold != "waiting_dependency" {
		t.Fatalf("a planning ticket with an open need = %s, hold %q; want planning on waiting_dependency", tk.State, tk.Hold)
	}
	if l, err := f.tasks.ClaimReady(ctx, "agent:planner-1", "planner"); err == nil {
		t.Fatalf("the held ticket's plan task was handed out: %+v", l)
	}
}

func TestAnInProgressTicketWhoseNeedsGainAnOpenTicketIsHeld(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	need := "gh:acme/web#2"
	if _, err := f.tickets.Create(ctx, "human:alex", need, "web"); err != nil {
		t.Fatal(err)
	}
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	if _, err := f.tickets.SetNeeds(ctx, "agent:implementer-1", ref, []string{need}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.State != core.TicketInProgress || tk.Hold != "waiting_dependency" {
		t.Fatalf("an in_progress ticket with an open need = %s, hold %q; want in_progress on waiting_dependency", tk.State, tk.Hold)
	}
	if l, err := f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer"); err == nil {
		t.Fatalf("the held ticket's implement task was handed out: %+v", l)
	}
}

func TestAHeldInProgressTicketGoesOnInItsRoundWhenItsNeedIsDone(t *testing.T) {
	t.Parallel()
	f := newEngine(t, config.Defaults{}, nil)
	need := "gh:acme/web#2"
	if _, err := f.tickets.Create(ctx, "human:alex", need, "web"); err != nil {
		t.Fatal(err)
	}
	f.readyTicket(t, ref)
	f.reconcile(t)
	f.finish(t, ref, "plan", "agent:planner-1")
	f.reconcile(t)
	if _, err := f.tickets.SetNeeds(ctx, "agent:implementer-1", ref, []string{need}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	for _, tr := range []string{"accept", "start", "plan_accepted", "pr_ready", "review_passed", "merged"} {
		if _, err := f.tickets.Fire(ctx, "system", need, tr); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := f.tasks.List(ctx)
	f.reconcile(t)
	if tk, _ := f.tickets.Get(ctx, ref); tk.State != core.TicketInProgress || tk.Hold != "" {
		t.Fatalf("after its need is done the ticket = %s, hold %q; want in_progress, no hold", tk.State, tk.Hold)
	}
	if after, _ := f.tasks.List(ctx); len(after) != len(before) {
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
	if cleared, _ := f.store.Read(ctx, "", store.Filter{Ticket: ref, TypePrefix: core.EventTicketHoldCleared}, 0); len(cleared) != 0 {
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
