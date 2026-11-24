package core_test

import (
	"slices"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
)

// ticketAt creates a ticket of the given priority and one implementer task
// for it; each call is a second later than the previous one.
func (f *fixture) ticketAt(t *testing.T, tickets *core.Tickets, n int, priority string, needs ...string) string {
	t.Helper()
	ref := "gh:acme/web#" + string(rune('0'+n))
	if _, err := tickets.Create(ctx, "human:alex", ref, "web"); err != nil {
		t.Fatal(err)
	}
	if priority != "" {
		if _, err := tickets.SetPriority(ctx, "human:alex", ref, priority); err != nil {
			t.Fatalf("SetPriority %s %s: %v", ref, priority, err)
		}
	}
	id := "t_" + string(rune('0'+n))
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{ID: id, Ticket: ref, Project: "web", Role: "implementer", Needs: needs}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Second)
	return id
}

func (f *fixture) claimOrder(t *testing.T, n int) []string {
	t.Helper()
	var order []string
	for range n {
		lease, err := f.tasks.ClaimReady(ctx, "agent:a", "implementer")
		if err != nil {
			t.Fatalf("ClaimReady after %v: %v", order, err)
		}
		order = append(order, lease.Task)
	}
	return order
}

func TestClaimReadyTakesTheHighestPriorityAndTheOldestOnATie(t *testing.T) {
	f := newFixture(t)
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	f.ticketAt(t, tickets, 1, "P2")
	f.ticketAt(t, tickets, 2, "P0")
	f.ticketAt(t, tickets, 3, "P3")
	f.ticketAt(t, tickets, 4, "P0")
	if got, want := f.claimOrder(t, 4), []string{"t_2", "t_4", "t_1", "t_3"}; !slices.Equal(got, want) {
		t.Fatalf("claim order = %v, want %v", got, want)
	}
}

func TestATaskNeededByAHigherPriorityTicketRunsAtThatPriority(t *testing.T) {
	f := newFixture(t)
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	f.ticketAt(t, tickets, 1, "P1")
	low := f.ticketAt(t, tickets, 2, "P3")
	f.ticketAt(t, tickets, 3, "P0", low)
	if got, want := f.claimOrder(t, 2), []string{"t_2", "t_1"}; !slices.Equal(got, want) {
		t.Fatalf("claim order = %v, want %v: the P3 task a P0 ticket waits for goes first", got, want)
	}
}

func TestOnlyAHumanChangesAPriority(t *testing.T) {
	f := newFixture(t)
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	ref := "gh:acme/web#1"
	if _, err := tickets.Create(ctx, "human:alex", ref, "web"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"agent:implementer-1", "system"} {
		_, err := tickets.SetPriority(ctx, actor, ref, "P0")
		assertCode(t, err, core.CodeHumanOnly)
	}
	tk, err := tickets.SetPriority(ctx, "human:alex", ref, "P0")
	if err != nil || tk.Priority != "P0" {
		t.Fatalf("SetPriority by a human = %+v, %v", tk, err)
	}
	if got, _ := tickets.Get(ctx, ref); got.Priority != "P0" {
		t.Fatalf("stored priority = %q, want P0", got.Priority)
	}
	if n := len(f.events(t, store.Filter{TypePrefix: "ticket.prioritized"})); n != 1 {
		t.Fatalf("ticket.prioritized events = %d, want 1", n)
	}
}

func TestAHigherPriorityDoesNotPreemptAClaimedTask(t *testing.T) {
	f := newFixture(t)
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	low := f.ticketAt(t, tickets, 1, "P3")
	lease := f.claim(t, low)
	f.ticketAt(t, tickets, 2, "P0")
	if got := f.state(t, low); got != core.TaskClaimed {
		t.Fatalf("the running P3 task is %s, want claimed", got)
	}
	if _, err := f.tasks.Renew(ctx, lease); err != nil {
		t.Fatalf("the running P3 task lost its lease: %v", err)
	}
	if got := f.claimOrder(t, 1); got[0] != "t_2" {
		t.Fatalf("next claim = %v, want the P0 task", got)
	}
}

func TestATicketWithoutAPriorityIsHandedOutAtTheConfiguredDefault(t *testing.T) {
	f := newFixture(t)
	f.tasks = core.NewTasks(f.store, core.TaskOptions{Clock: f.clock.Now, DefaultPriority: "P1"})
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	f.ticketAt(t, tickets, 1, "P2")
	f.ticketAt(t, tickets, 2, "")
	if got, want := f.claimOrder(t, 2), []string{"t_2", "t_1"}; !slices.Equal(got, want) {
		t.Fatalf("claim order = %v, want %v: no priority means the default P1", got, want)
	}
}

func TestSettingTheSamePriorityAgainChangesNothing(t *testing.T) {
	f := newFixture(t)
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	f.ticketAt(t, tickets, 1, "P1")
	if _, err := tickets.SetPriority(ctx, "human:alex", "gh:acme/web#1", "P1"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.events(t, store.Filter{TypePrefix: "ticket.prioritized"})); n != 1 {
		t.Fatalf("ticket.prioritized events = %d, want 1", n)
	}
}

func TestClaimReadyInTakesOnlyTasksOfTheNamedProjects(t *testing.T) {
	f := newFixture(t)
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{ID: "t_api", Ticket: "gh:acme/api#1", Project: "api", Role: "implementer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.ClaimReadyIn(ctx, "agent:a", "implementer", []string{"web"}); err == nil {
		t.Fatal("a task of another project was claimed")
	}
	if lease, err := f.tasks.ClaimReadyIn(ctx, "agent:a", "implementer", []string{"web", "api"}); err != nil || lease.Task != "t_api" {
		t.Fatalf("ClaimReadyIn = %+v, %v; want t_api", lease, err)
	}
}
