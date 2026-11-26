package runner_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/runner"
)

var budgets = map[string]float64{"XS": 2, "S": 5, "M": 15, "L": 30}

func sized(t *testing.T, f *runFixture, ref, size string) {
	t.Helper()
	if _, err := f.tickets.SetSize(ctx, "system", ref, size); err != nil {
		t.Fatal(err)
	}
}

func TestARunIsCappedAtWhatIsLeftOfItsTicketsBudget(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, _ := f.readyTask(t, 1)
	sized(t, f, tk.Ref, "S")
	f.start(t)
	first := f.session(t)
	if first.spec.MaxBudgetUSD != 5 {
		t.Fatalf("the first run of an S ticket is capped at $%v, want $5", first.spec.MaxBudgetUSD)
	}
	first.finish <- harness.Result{ExitCode: 1} // spent $2, failed
	second := f.session(t)
	if second.spec.MaxBudgetUSD != 3 {
		t.Fatalf("after $2 spent the next run is capped at $%v, want $3", second.spec.MaxBudgetUSD)
	}
	second.Stop(0)
}

func TestNoRunStartsWhenTheTicketsBudgetIsSpent(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, task := f.readyTask(t, 2)
	sized(t, f, tk.Ref, "XS")
	f.start(t)
	f.session(t).finish <- harness.Result{ExitCode: 1} // spent all $2
	if blocked := f.waitTask(t, task.ID, core.TaskBlocked); blocked.BlockKind != "budget_exceeded" {
		t.Fatalf("block kind = %q, want budget_exceeded", blocked.BlockKind)
	}
	select {
	case s := <-f.h.started:
		t.Fatalf("a run started with the budget spent: %+v", s.spec.MaxBudgetUSD)
	default:
	}
}

func TestATaskBlockedForBudgetLeavesItsTicketHeld(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, task := f.readyTask(t, 4)
	sized(t, f, tk.Ref, "XS")
	f.start(t)
	f.session(t).finish <- harness.Result{ExitCode: 1} // spent all $2
	f.waitTask(t, task.ID, core.TaskBlocked)
	// The hold follows the block; under parallel tests it can land later.
	f.waitEvent(t, core.EventTicketHoldSet)
	held, err := f.tickets.Get(ctx, tk.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if held.Hold != "budget_exceeded" {
		t.Fatalf("hold = %q, want budget_exceeded", held.Hold)
	}
}

func TestTicketsThatShareABudgetSpendItTogether(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 4}})
	parent, task := f.readyTask(t, 3)
	sized(t, f, parent.Ref, "M")
	f.start(t)
	s := f.session(t) // the parent's run spends $4 and is done
	if _, err := f.runner.Runs().Complete(ctx, s.spec.Env["ISTARI_RUN"], "done"); err != nil {
		t.Fatal(err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	f.waitTask(t, task.ID, core.TaskCompleted)
	// The parent's implementer files a ticket (istari ticket create): not a
	// child of a split, so it shares the parent's budget.
	child, err := f.tickets.CreateWith(ctx, "agent:implementer-1", core.NewTicket{Ref: "gh:acme/web#30", Project: "web", Title: "Found", BudgetOf: parent.Ref})
	if err != nil {
		t.Fatal(err)
	}
	sized(t, f, child.Ref, "S")
	f.fire(t, child.Ref, "accept", "start")
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: child.Ref, Project: "web", Role: "implementer"}); err != nil {
		t.Fatal(err)
	}
	c := f.session(t)
	defer c.Stop(0)
	if c.spec.MaxBudgetUSD != 11 {
		t.Fatalf("the child's run is capped at $%v, want the parent's M budget of $15 less the $4 spent", c.spec.MaxBudgetUSD)
	}
}

func TestAGrandchildSharesTheBudgetOfTheFirstTicket(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 4}})
	first := f.ticket(t, 40, "First")
	sized(t, f, first.Ref, "L")
	var chain []string
	ref := first.Ref
	for n, size := range []string{"S", "XS"} { // a child, then its child
		tk, err := f.tickets.CreateWith(ctx, "agent:planner-1", core.NewTicket{Ref: fmt.Sprintf("gh:acme/web#%d", 41+n), Project: "web", BudgetOf: ref})
		if err != nil {
			t.Fatal(err)
		}
		sized(t, f, tk.Ref, size)
		f.fire(t, tk.Ref, "accept", "start")
		ref = tk.Ref
		chain = append(chain, ref)
	}
	f.start(t)
	// The child spends $4 and is done.
	task, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: chain[0], Project: "web", Role: "implementer"})
	if err != nil {
		t.Fatal(err)
	}
	s := f.session(t)
	if _, err := f.runner.Runs().Complete(ctx, s.spec.Env["ISTARI_RUN"], "done"); err != nil {
		t.Fatal(err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	f.waitTask(t, task.ID, core.TaskCompleted)
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: chain[1], Project: "web", Role: "implementer"}); err != nil {
		t.Fatal(err)
	}
	g := f.session(t)
	defer g.Stop(0)
	if g.spec.MaxBudgetUSD != 26 {
		t.Fatalf("the grandchild's run is capped at $%v, want the first ticket's L budget of $30 less the child's $4", g.spec.MaxBudgetUSD)
	}
}

// spendAndFinish lets a ticket's task run once, spend what the harness
// reports and complete.
func spendAndFinish(t *testing.T, f *runFixture, taskID string) {
	t.Helper()
	s := f.session(t)
	if _, err := f.runner.Runs().Complete(ctx, s.spec.Env["ISTARI_RUN"], "done"); err != nil {
		t.Fatal(err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	f.waitTask(t, taskID, core.TaskCompleted)
}

// split splits parent into a child with the given number and size, as the
// planner does (T5), and starts the child.
func split(t *testing.T, f *runFixture, parent string, n int, size string) core.Ticket {
	t.Helper()
	child, err := f.tickets.CreateWith(ctx, "agent:planner-1", core.NewTicket{Ref: fmt.Sprintf("gh:acme/web#%d", n), Project: "web", Title: "Child", BudgetOf: parent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tickets.SetChildren(ctx, "agent:planner-1", parent, []string{child.Ref}); err != nil {
		t.Fatal(err)
	}
	sized(t, f, child.Ref, size)
	f.fire(t, child.Ref, "accept", "start")
	return child
}

func implement(t *testing.T, f *runFixture, ref string) core.Task {
	t.Helper()
	task, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: ref, Project: "web", Role: "implementer"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestAChildOfASplitHasABudgetOfItsOwnSize(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 4}})
	parent, task := f.readyTask(t, 50)
	sized(t, f, parent.Ref, "M")
	f.start(t)
	spendAndFinish(t, f, task.ID) // the planner spends $4 on the parent
	sized(t, f, parent.Ref, "XL") // too large: the planner splits it
	child := split(t, f, parent.Ref, 51, "S")
	implement(t, f, child.Ref)
	c := f.session(t)
	defer c.Stop(0)
	if c.spec.MaxBudgetUSD != 5 {
		t.Fatalf("the split child's run is capped at $%v, want its own S budget of $5", c.spec.MaxBudgetUSD)
	}
}

func TestATicketFiledWhileWorkingOnASplitChildSharesTheChildsBudget(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 4}})
	parent, task := f.readyTask(t, 60)
	sized(t, f, parent.Ref, "M")
	f.start(t)
	spendAndFinish(t, f, task.ID) // the planner spends $4 on the parent
	sized(t, f, parent.Ref, "XL")
	child := split(t, f, parent.Ref, 61, "S")
	spendAndFinish(t, f, implement(t, f, child.Ref).ID) // the child spends $4 of its $5
	// The child's implementer files a ticket (istari ticket create).
	filed, err := f.tickets.CreateWith(ctx, "agent:implementer-1", core.NewTicket{Ref: "gh:acme/web#62", Project: "web", Title: "Found", BudgetOf: child.Ref})
	if err != nil {
		t.Fatal(err)
	}
	sized(t, f, filed.Ref, "L")
	f.fire(t, filed.Ref, "accept", "start")
	implement(t, f, filed.Ref)
	g := f.session(t)
	defer g.Stop(0)
	if g.spec.MaxBudgetUSD != 1 {
		t.Fatalf("the filed ticket's run is capped at $%v, want the child's S budget of $5 less the child's $4", g.spec.MaxBudgetUSD)
	}
}

// spendTheBudget lets an XS ticket's only run spend all of its $2, so its
// task is blocked budget_exceeded and the ticket is held.
func spendTheBudget(t *testing.T, f *runFixture, n int) (core.Ticket, core.Task) {
	t.Helper()
	tk, task := f.readyTask(t, n)
	sized(t, f, tk.Ref, "XS")
	f.start(t)
	f.session(t).finish <- harness.Result{ExitCode: 1} // spent all $2
	f.waitTask(t, task.ID, core.TaskBlocked)
	f.waitEvent(t, core.EventTicketHoldSet)
	return tk, task
}

func TestLiftingABudgetHoldAfterRaisingTheSizeRunsTheTaskAgain(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, _ := spendTheBudget(t, f, 70)
	sized(t, f, tk.Ref, "S")
	lifted, err := f.runner.LiftBudgetHold(ctx, "human:alex", tk.Ref, false)
	if err != nil {
		t.Fatal(err)
	}
	if lifted.Hold != "" {
		t.Fatalf("hold = %q after lifting, want none", lifted.Hold)
	}
	s := f.session(t) // the task is ready again and handed out
	defer s.Stop(0)
	if s.spec.MaxBudgetUSD != 3 {
		t.Fatalf("the next run is capped at $%v, want the S budget of $5 less the $2 spent", s.spec.MaxBudgetUSD)
	}
}

func TestABudgetHoldIsNotLiftedWhileTheBudgetIsStillSpent(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, task := spendTheBudget(t, f, 71)
	_, err := f.runner.LiftBudgetHold(ctx, "human:alex", tk.Ref, false)
	var refused *core.Error
	if !errors.As(err, &refused) || refused.Code != core.CodeInvalidState {
		t.Fatalf("LiftBudgetHold with the size unchanged = %v, want invalid_state", err)
	}
	for _, want := range []string{"size label", "--anyway"} {
		if !strings.Contains(refused.Message, want) {
			t.Errorf("refusal %q does not name %q", refused.Message, want)
		}
	}
	if held, _ := f.tickets.Get(ctx, tk.Ref); held.Hold != "budget_exceeded" {
		t.Fatalf("hold = %q after the refusal, want budget_exceeded", held.Hold)
	}
	if got, _ := f.tasks.Get(ctx, task.ID); got.State != core.TaskBlocked {
		t.Fatalf("task is %s after the refusal, want blocked", got.State)
	}
}

func TestLiftingABudgetHoldRefusesAnotherHoldAndNamesItsCommand(t *testing.T) {
	t.Parallel()
	for n, c := range []struct{ hold, command string }{
		{"paused", "istari ticket resume"},
		{"attempts_exhausted", "istari ticket retry"},
	} {
		t.Run(c.hold, func(t *testing.T) {
			t.Parallel()
			f := newRunner(t, runner.RunnerOptions{Budgets: budgets})
			tk, _ := f.readyTask(t, 80+n)
			if _, err := f.tickets.SetHold(ctx, "system", tk.Ref, c.hold); err != nil {
				t.Fatal(err)
			}
			_, err := f.runner.LiftBudgetHold(ctx, "human:alex", tk.Ref, true)
			var refused *core.Error
			if !errors.As(err, &refused) || refused.Code != core.CodeInvalidState || !strings.Contains(refused.Message, c.command) {
				t.Fatalf("LiftBudgetHold on a %s hold = %v, want invalid_state naming %s", c.hold, err, c.command)
			}
			if held, _ := f.tickets.Get(ctx, tk.Ref); held.Hold != c.hold {
				t.Fatalf("hold = %q after the refusal, want %s", held.Hold, c.hold)
			}
		})
	}
}

func TestABudgetHoldIsLiftedAnywayWithoutRaisingTheSize(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, task := spendTheBudget(t, f, 72)
	lifted, err := f.runner.LiftBudgetHold(ctx, "human:alex", tk.Ref, true)
	if err != nil {
		t.Fatal(err)
	}
	if lifted.Hold != "" {
		t.Fatalf("hold = %q after lifting anyway, want none", lifted.Hold)
	}
	unblocked := f.waitEvent(t, "task.unblocked")
	if !strings.Contains(string(unblocked.Payload), "budget_exceeded") {
		t.Fatalf("task.unblocked = %s, want the budget_exceeded block of %s lifted", unblocked.Payload, task.ID)
	}
}

func TestLiftingABudgetHoldAnywayLetsTheNextRunStart(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, _ := spendTheBudget(t, f, 73)
	if _, err := f.runner.LiftBudgetHold(ctx, "human:alex", tk.Ref, true); err != nil {
		t.Fatal(err)
	}
	s := f.session(t)
	defer s.Stop(0)
	if s.spec.MaxBudgetUSD != 2 {
		t.Fatalf("the run let start anyway is capped at $%v, want the XS budget of $2", s.spec.MaxBudgetUSD)
	}
	if held, _ := f.tickets.Get(ctx, tk.Ref); held.Hold != "" {
		t.Fatalf("hold = %q while the run let start anyway runs, want none", held.Hold)
	}
}

func TestARunLetStartAnywayStartsAfterADaemonRestart(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, task := f.readyTask(t, 75)
	sized(t, f, tk.Ref, "XS")
	stop := f.start(t)
	f.session(t).finish <- harness.Result{ExitCode: 1} // spent all $2
	f.waitTask(t, task.ID, core.TaskBlocked)
	f.waitEvent(t, core.EventTicketHoldSet)
	stop()
	if _, err := f.runner.LiftBudgetHold(ctx, "human:alex", tk.Ref, true); err != nil {
		t.Fatal(err)
	}
	// The daemon restarts: a new runner over the same store.
	f.runner = runner.New(f.store, f.tasks, f.tickets, runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + f.root + "/remotes"}), runner.RunnerOptions{
		Budgets: budgets, DataDir: f.data, Projects: []string{"web"}, Lease: time.Minute,
		Slots: []runner.Slot{{Profile: "implementer-1", Role: "implementer", Harness: harness.Profile{Harness: f.h.name}}},
	})
	f.start(t)
	s := f.session(t)
	defer s.Stop(0)
	if s.spec.MaxBudgetUSD != 2 {
		t.Fatalf("the run let start anyway is capped at $%v after a restart, want the XS budget of $2", s.spec.MaxBudgetUSD)
	}
}

func TestAnUnholdAnywayIsUsedUpByTheNextRunEvenWithBudgetLeft(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, task := f.readyTask(t, 76)
	sized(t, f, tk.Ref, "XS")
	stop := f.start(t)
	f.session(t).finish <- harness.Result{ExitCode: 1} // spent all $2
	f.waitTask(t, task.ID, core.TaskBlocked)
	f.waitEvent(t, core.EventTicketHoldSet)
	stop()
	if _, err := f.runner.LiftBudgetHold(ctx, "human:alex", tk.Ref, true); err != nil {
		t.Fatal(err)
	}
	sized(t, f, tk.Ref, "S") // before the next claim: $3 left
	f.start(t)
	spendAndFinish(t, f, task.ID) // a run with budget left, $4 spent in all
	sized(t, f, tk.Ref, "XS")    // spent again
	next := implement(t, f, tk.Ref)
	if blocked := f.waitTask(t, next.ID, core.TaskBlocked); blocked.BlockKind != "budget_exceeded" {
		t.Fatalf("block kind = %q, want budget_exceeded", blocked.BlockKind)
	}
	select {
	case s := <-f.h.started:
		t.Fatalf("an unhold --anyway that no run needed let a later run start: capped at $%v", s.spec.MaxBudgetUSD)
	case <-time.After(time.Second):
	}
}

func TestLiftingABudgetHoldAnywayLetsOnlyOneRunStart(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets}, harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 2}})
	tk, task := spendTheBudget(t, f, 74)
	if _, err := f.runner.LiftBudgetHold(ctx, "human:alex", tk.Ref, true); err != nil {
		t.Fatal(err)
	}
	f.session(t).finish <- harness.Result{ExitCode: 1} // spent another $2, failed
	if blocked := f.waitTask(t, task.ID, core.TaskBlocked); blocked.BlockKind != "budget_exceeded" {
		t.Fatalf("block kind = %q after the run let start anyway, want budget_exceeded", blocked.BlockKind)
	}
	// The hold follows the block; under parallel tests it can land later.
	for deadline := time.Now().Add(waitFor); ; time.Sleep(20 * time.Millisecond) {
		held, _ := f.tickets.Get(ctx, tk.Ref)
		if held.Hold == "budget_exceeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hold = %q after the run let start anyway, want budget_exceeded", held.Hold)
		}
	}
	select {
	case s := <-f.h.started:
		t.Fatalf("a second run started after one unhold --anyway: capped at $%v", s.spec.MaxBudgetUSD)
	default:
	}
}
