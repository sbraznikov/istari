package runner_test

import (
	"fmt"
	"testing"

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

func TestTicketsThatShareABudgetSpendItTogether(t *testing.T) {
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
	child, err := f.tickets.CreateWith(ctx, "agent:planner-1", core.NewTicket{Ref: "gh:acme/web#30", Project: "web", Title: "Child", BudgetOf: parent.Ref})
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
	f := newRunner(t, runner.RunnerOptions{Budgets: budgets})
	first := f.ticket(t, 40, "First")
	sized(t, f, first.Ref, "L")
	ref := first.Ref
	for n, size := range []string{"S", "XS"} { // a child, then its child
		tk, err := f.tickets.CreateWith(ctx, "agent:planner-1", core.NewTicket{Ref: fmt.Sprintf("gh:acme/web#%d", 41+n), Project: "web", BudgetOf: ref})
		if err != nil {
			t.Fatal(err)
		}
		sized(t, f, tk.Ref, size)
		ref = tk.Ref
	}
	f.fire(t, ref, "accept", "start")
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: ref, Project: "web", Role: "implementer"}); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	s := f.session(t)
	defer s.Stop(0)
	if s.spec.MaxBudgetUSD != 30 {
		t.Fatalf("the grandchild's run is capped at $%v, want the first ticket's L budget of $30", s.spec.MaxBudgetUSD)
	}
}
