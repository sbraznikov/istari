package pipeline_test

import (
	"context"
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
	f := &engineFixture{tasks: core.NewTasks(s, core.TaskOptions{}), tickets: core.NewTickets(s, core.TicketOptions{})}
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
