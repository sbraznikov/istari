package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
)

const ref = "gh:acme/web#123"

func newTickets(t *testing.T, guards core.Guards) (*core.Tickets, *store.SQLite) {
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
	clock := func() time.Time { return time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC) }
	return core.NewTickets(s, core.TicketOptions{Clock: clock, Guards: guards}), s
}

// The transition table of spec section 4.1, written out independently of the code.
var transitionTable = []struct {
	id      string
	from    core.TicketState
	trigger string
	to      core.TicketState
}{
	{"T2", core.TicketNew, "accept", core.TicketReady},
	{"T3", core.TicketReady, "start", core.TicketPlanning},
	{"T4", core.TicketPlanning, "plan_accepted", core.TicketInProgress},
	{"T5", core.TicketPlanning, "split", core.TicketInProgress},
	{"T6", core.TicketInProgress, "pr_ready", core.TicketInReview},
	{"T7", core.TicketInReview, "changes_requested", core.TicketInProgress},
	{"T8", core.TicketInReview, "review_passed", core.TicketAwaitingApproval},
	{"T9", core.TicketAwaitingApproval, "changes_requested", core.TicketInProgress},
	{"T10", core.TicketAwaitingApproval, "merge_failed", core.TicketInProgress},
	{"T10", core.TicketInReview, "merge_failed", core.TicketInProgress},
	{"T11", core.TicketAwaitingApproval, "merged", core.TicketDone},
	{"T11", core.TicketInProgress, "merged", core.TicketDone},
	{"T11", core.TicketInReview, "merged", core.TicketDone},
	{"T11", core.TicketCanceled, "merged", core.TicketDone},
	{"T12", core.TicketInProgress, "children_done", core.TicketDone},
	{"T13", core.TicketNew, "cancel", core.TicketCanceled},
	{"T13", core.TicketReady, "cancel", core.TicketCanceled},
	{"T13", core.TicketPlanning, "cancel", core.TicketCanceled},
	{"T13", core.TicketInProgress, "cancel", core.TicketCanceled},
	{"T13", core.TicketInReview, "cancel", core.TicketCanceled},
	{"T13", core.TicketAwaitingApproval, "cancel", core.TicketCanceled},
	{"T14", core.TicketCanceled, "reopen", core.TicketNew},
	{"T15", core.TicketReady, "edited", core.TicketNew},
	{"T16", core.TicketNew, "complete", core.TicketDone},
	{"T16", core.TicketReady, "complete", core.TicketDone},
	{"T16", core.TicketPlanning, "complete", core.TicketDone},
	{"T16", core.TicketInProgress, "complete", core.TicketDone},
	{"T16", core.TicketInReview, "complete", core.TicketDone},
	{"T16", core.TicketAwaitingApproval, "complete", core.TicketDone},
	{"T16", core.TicketCanceled, "complete", core.TicketDone},
}

// pathTo lists triggers that bring a new ticket to a state.
var pathTo = map[core.TicketState][]string{
	core.TicketNew:              {},
	core.TicketReady:            {"accept"},
	core.TicketPlanning:         {"accept", "start"},
	core.TicketInProgress:       {"accept", "start", "plan_accepted"},
	core.TicketInReview:         {"accept", "start", "plan_accepted", "pr_ready"},
	core.TicketAwaitingApproval: {"accept", "start", "plan_accepted", "pr_ready", "review_passed"},
	core.TicketDone:             {"accept", "start", "plan_accepted", "pr_ready", "review_passed", "merged"},
	core.TicketCanceled:         {"cancel"},
}

func bringTo(t *testing.T, tickets *core.Tickets, state core.TicketState) {
	t.Helper()
	if _, err := tickets.Create(ctx, "human:alex", ref, "web"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, trigger := range pathTo[state] {
		if _, err := tickets.Fire(ctx, "system", ref, trigger); err != nil {
			t.Fatalf("bringing to %s, %s: %v", state, trigger, err)
		}
	}
}

func TestEveryRowOfTheTransitionTable(t *testing.T) {
	t.Parallel()
	for _, row := range transitionTable {
		t.Run(row.id+"_"+string(row.from)+"_"+row.trigger, func(t *testing.T) {
			t.Parallel()
			tickets, s := newTickets(t, nil)
			bringTo(t, tickets, row.from)
			before, _ := s.Read(ctx, "", store.Filter{}, 0)
			ticket, err := tickets.Fire(ctx, "human:alex", ref, row.trigger)
			if err != nil {
				t.Fatalf("Fire: %v", err)
			}
			if ticket.State != row.to {
				t.Fatalf("state = %s, want %s", ticket.State, row.to)
			}
			after, _ := s.Read(ctx, "", store.Filter{}, 0)
			if len(after) != len(before)+1 {
				t.Fatalf("appended %d events, want 1", len(after)-len(before))
			}
			e := after[len(after)-1]
			var p struct{ From, To, Trigger, Actor string }
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if e.Type != "ticket.transitioned" || e.Ticket != ref || p.From != string(row.from) ||
				p.To != string(row.to) || p.Trigger != row.trigger || p.Actor != "human:alex" {
				t.Fatalf("event = %s %s %+v", e.Type, e.Ticket, p)
			}
		})
	}
}

// Two rows lead from in_review and from awaiting_approval to in_progress; a
// tracker label edit reads them as changes_requested, not merge_failed.
func TestTriggerToReturnsToInProgressByChangesRequested(t *testing.T) {
	t.Parallel()
	for _, from := range []core.TicketState{core.TicketInReview, core.TicketAwaitingApproval} {
		if got, ok := core.TriggerTo(from, core.TicketInProgress); !ok || got != "changes_requested" {
			t.Fatalf("TriggerTo(%s, in_progress) = %q, %v; want changes_requested", from, got, ok)
		}
	}
}

func TestCreateIsT1(t *testing.T) {
	t.Parallel()
	tickets, s := newTickets(t, nil)
	ticket, err := tickets.Create(ctx, "human:alex", ref, "web")
	if err != nil || ticket.State != core.TicketNew {
		t.Fatalf("Create = %s, %v; want new", ticket.State, err)
	}
	events, _ := s.Read(ctx, "", store.Filter{Ticket: ref}, 0)
	if len(events) != 1 || events[0].Type != "ticket.transitioned" {
		t.Fatalf("events = %v", events)
	}
	if _, err := tickets.Create(ctx, "human:alex", ref, "web"); err == nil {
		t.Fatal("creating the same ticket twice succeeded")
	}
}

var allStates = []core.TicketState{
	core.TicketNew, core.TicketReady, core.TicketPlanning, core.TicketInProgress,
	core.TicketInReview, core.TicketAwaitingApproval, core.TicketDone, core.TicketCanceled,
}

var allTriggers = []string{
	"accept", "start", "plan_accepted", "split", "pr_ready", "changes_requested", "review_passed",
	"merge_failed", "merged", "children_done", "cancel", "reopen", "edited", "complete", "create", "approve",
}

func inTable(from core.TicketState, trigger string) bool {
	for _, row := range transitionTable {
		if row.from == from && row.trigger == trigger {
			return true
		}
	}
	return false
}

func TestEveryPairOutsideTheTableIsRejectedAndAppendsNothing(t *testing.T) {
	t.Parallel()
	for _, from := range allStates {
		for _, trigger := range allTriggers {
			if inTable(from, trigger) {
				continue
			}
			tickets, s := newTickets(t, nil)
			bringTo(t, tickets, from)
			before, _ := s.Read(ctx, "", store.Filter{}, 0)
			_, err := tickets.Fire(ctx, "human:alex", ref, trigger)
			assertCode(t, err, core.CodeInvalidTransition)
			after, _ := s.Read(ctx, "", store.Filter{}, 0)
			if len(after) != len(before) {
				t.Fatalf("%s + %s appended %d events", from, trigger, len(after)-len(before))
			}
			if got, _ := tickets.Get(ctx, ref); got.State != from {
				t.Fatalf("%s + %s changed the state to %s", from, trigger, got.State)
			}
		}
	}
}

func TestACanceledTicketReopensToNew(t *testing.T) {
	t.Parallel()
	tickets, _ := newTickets(t, nil)
	bringTo(t, tickets, core.TicketCanceled)
	if got, err := tickets.Fire(ctx, "human:alex", ref, "reopen"); err != nil || got.State != core.TicketNew {
		t.Fatalf("reopen = %s, %v; want new", got.State, err)
	}
}

func TestAHoldCanBeSetOnAnyUnfinishedStateAndLeavesTheStateAlone(t *testing.T) {
	t.Parallel()
	for _, state := range allStates {
		tickets, _ := newTickets(t, nil)
		bringTo(t, tickets, state)
		held, err := tickets.SetHold(ctx, "system", ref, "paused")
		if state == core.TicketDone || state == core.TicketCanceled {
			assertCode(t, err, core.CodeInvalidState)
			continue
		}
		if err != nil {
			t.Fatalf("SetHold on %s: %v", state, err)
		}
		if held.State != state || held.Hold != "paused" {
			t.Fatalf("held ticket = %s/%s, want %s/paused", held.State, held.Hold, state)
		}
		cleared, err := tickets.ClearHold(ctx, "human:alex", ref, "")
		if err != nil || cleared.State != state || cleared.Hold != "" {
			t.Fatalf("ClearHold on %s = %s/%q, %v", state, cleared.State, cleared.Hold, err)
		}
	}
}

func TestHoldReasonsAndHeldTransitions(t *testing.T) {
	t.Parallel()
	tickets, _ := newTickets(t, nil)
	bringTo(t, tickets, core.TicketReady)
	if _, err := tickets.SetHold(ctx, "system", ref, "sleepy"); err == nil {
		t.Fatal("SetHold accepted an unknown reason")
	}
	for _, reason := range []string{"needs_input", "attempts_exhausted", "budget_exceeded", "paused", "waiting_dependency", "waiting_zone"} {
		if _, err := tickets.SetHold(ctx, "system", ref, reason); err != nil {
			t.Fatalf("SetHold %s: %v", reason, err)
		}
		if _, err := tickets.ClearHold(ctx, "system", ref, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tickets.SetHold(ctx, "system", ref, "waiting_zone"); err != nil {
		t.Fatal(err)
	}
	_, err := tickets.Fire(ctx, "system", ref, "start")
	assertCode(t, err, core.CodeTicketHeld)
	// Cancel passes a hold and ends it: a finished ticket carries no hold.
	got, err := tickets.Fire(ctx, "human:alex", ref, "cancel")
	if err != nil || got.State != core.TicketCanceled || got.Hold != "" {
		t.Fatalf("cancel while held = %s/%q, %v; want canceled without a hold", got.State, got.Hold, err)
	}
	if got, err := tickets.Fire(ctx, "human:alex", ref, "reopen"); err != nil || got.State != core.TicketNew || got.Hold != "" {
		t.Fatalf("reopen after cancel = %s/%q, %v; want new without a hold", got.State, got.Hold, err)
	}
	// So does a human's close as completed (T16).
	if _, err := tickets.SetHold(ctx, "system", ref, "paused"); err != nil {
		t.Fatal(err)
	}
	if got, err := tickets.Fire(ctx, "human:alex", ref, "complete"); err != nil || got.State != core.TicketDone || got.Hold != "" {
		t.Fatalf("complete while held = %s/%q, %v; want done without a hold", got.State, got.Hold, err)
	}
}

func TestAMergedPullRequestFinishesAHeldTicketAndClearsItsHold(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"attempts_exhausted", "budget_exceeded", "paused"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			tickets, s := newTickets(t, nil)
			bringTo(t, tickets, core.TicketInReview)
			if _, err := tickets.SetHold(ctx, "system", ref, reason); err != nil {
				t.Fatal(err)
			}
			_, err := tickets.Fire(ctx, "system", ref, "review_passed")
			assertCode(t, err, core.CodeTicketHeld)
			got, err := tickets.Fire(ctx, "system", ref, "merged")
			if err != nil || got.State != core.TicketDone || got.Hold != "" {
				t.Fatalf("merged while held = %s/%q, %v; want done without a hold", got.State, got.Hold, err)
			}
			events, _ := s.Read(ctx, "", store.Filter{Ticket: ref}, 0)
			last := events[len(events)-1]
			var payload map[string]any
			if err := json.Unmarshal(last.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if last.Type != core.EventTicketTransitioned || payload["hold_cleared"] != reason {
				t.Fatalf("last event = %s %s, want ticket.transitioned with hold_cleared %s", last.Type, last.Payload, reason)
			}
			replayed, err := core.ReplayTicket(events)
			if err != nil || replayed.State != core.TicketDone || replayed.Hold != "" {
				t.Fatalf("replay = %s/%q, %v; want done without a hold", replayed.State, replayed.Hold, err)
			}
		})
	}
}

func TestMissingTicketsAndMissingHolds(t *testing.T) {
	t.Parallel()
	tickets, _ := newTickets(t, nil)
	_, err := tickets.Fire(ctx, "system", ref, "accept")
	assertCode(t, err, core.CodeNotFound)
	bringTo(t, tickets, core.TicketReady)
	_, err = tickets.ClearHold(ctx, "system", ref, "")
	assertCode(t, err, core.CodeInvalidState)
}

func TestTwoConcurrentTransitionsOfOneTicketExactlyOneWins(t *testing.T) {
	t.Parallel()
	for round := range 20 {
		tickets, s := newTickets(t, nil)
		bringTo(t, tickets, core.TicketReady)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = tickets.Fire(ctx, "system", ref, "start")
			}()
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			if err == nil {
				won++
			} else {
				assertCode(t, err, core.CodeInvalidTransition)
			}
		}
		events, err := s.Read(ctx, "", store.Filter{Ticket: ref, TypePrefix: "ticket.transitioned"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if won != 1 || len(events) != 3 { // create, accept, one start
			t.Fatalf("round %d: %d transitions won, %d events; want 1 and 3", round, won, len(events))
		}
	}
}

func TestAGuardKeyThatIsNoTransitionIsRejected(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("NewTickets accepted a guard for the unknown transition t3")
		}
	}()
	newTickets(t, core.Guards{"t3": func(context.Context, core.Ticket) error { return nil }})
}

func TestReplayRejectsAnInvalidHistory(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	ev := func(ticket, typ, payload string) store.Event {
		return store.Event{Type: typ, Ticket: ticket, At: at, Payload: json.RawMessage(payload)}
	}
	create := ev(ref, "ticket.transitioned", `{"from":"","to":"new","trigger":"create"}`)
	for name, events := range map[string][]store.Event{
		"transition outside the table": {create, ev(ref, "ticket.transitioned", `{"from":"new","to":"done","trigger":"merged"}`)},
		"hold before create":           {ev(ref, "ticket.hold_set", `{"reason":"paused"}`), create},
		"events of two tickets":        {create, ev("gh:acme/web#999", "ticket.transitioned", `{"from":"new","to":"ready","trigger":"accept"}`)},
		"history without create":       {ev(ref, "ticket.transitioned", `{"from":"","to":"ready","trigger":"accept"}`)},
	} {
		if _, err := core.ReplayTicket(events); err == nil {
			t.Errorf("%s: replay succeeded", name)
		}
	}
}

func TestAGuardThatRefusesBlocksItsTransitionOnly(t *testing.T) {
	t.Parallel()
	refuse := errors.New("there is no free slot")
	tickets, s := newTickets(t, core.Guards{
		"T3": func(context.Context, core.Ticket) error { return refuse },
	})
	bringTo(t, tickets, core.TicketReady)
	before, _ := s.Read(ctx, "", store.Filter{}, 0)
	_, err := tickets.Fire(ctx, "system", ref, "start")
	assertCode(t, err, core.CodeGuardFailed)
	if after, _ := s.Read(ctx, "", store.Filter{}, 0); len(after) != len(before) {
		t.Fatal("a refused guard appended an event")
	}
	if got, err := tickets.Fire(ctx, "system", ref, "edited"); err != nil || got.State != core.TicketNew {
		t.Fatalf("T15 with no guard = %s, %v", got.State, err)
	}
}

func TestTheHistoryOfATicketCanBeRebuiltFromTheLog(t *testing.T) {
	t.Parallel()
	tickets, s := newTickets(t, nil)
	for _, r := range []string{ref, "gh:acme/web#999"} { // another ticket shares the log
		if _, err := tickets.Create(ctx, "human:alex", r, "web"); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []func() (core.Ticket, error){
		func() (core.Ticket, error) { return tickets.Fire(ctx, "system", ref, "accept") },
		func() (core.Ticket, error) { return tickets.Fire(ctx, "system", ref, "start") },
		func() (core.Ticket, error) { return tickets.SetPriority(ctx, "human:alex", ref, "P0") },
		func() (core.Ticket, error) { return tickets.SetSize(ctx, "agent:planner-1", ref, "M") },
		func() (core.Ticket, error) { return tickets.SetNeeds(ctx, "system", ref, []string{"gh:acme/web#999"}) },
		func() (core.Ticket, error) { return tickets.SetHold(ctx, "system", ref, "waiting_zone") },
		func() (core.Ticket, error) { return tickets.ClearHold(ctx, "system", ref, "") },
		func() (core.Ticket, error) { return tickets.Fire(ctx, "system", ref, "plan_accepted") },
		func() (core.Ticket, error) { return tickets.SetHold(ctx, "system", ref, "needs_input") },
	} {
		projected, err := step()
		if err != nil {
			t.Fatal(err)
		}
		events, _ := s.Read(ctx, "", store.Filter{Ticket: ref}, 0)
		replayed, err := core.ReplayTicket(events)
		if err != nil {
			t.Fatalf("ReplayTicket: %v", err)
		}
		if replayed.State != projected.State || replayed.Hold != projected.Hold || replayed.Priority != projected.Priority || replayed.Size != projected.Size || strings.Join(replayed.Needs, ",") != strings.Join(projected.Needs, ",") {
			t.Fatalf("after step to %s/%q/%q/%q the replay gives %s/%q/%q/%q", projected.State, projected.Hold, projected.Priority, projected.Size, replayed.State, replayed.Hold, replayed.Priority, replayed.Size)
		}
	}
	events, _ := s.Read(ctx, "", store.Filter{Ticket: ref}, 0)
	replayed, err := core.ReplayTicket(events)
	if err != nil {
		t.Fatalf("ReplayTicket: %v", err)
	}
	if replayed.Ref != ref || replayed.Project != "web" || replayed.State != core.TicketInProgress || replayed.Hold != "needs_input" || replayed.Priority != "P0" || replayed.Size != "M" {
		t.Fatalf("replayed = %+v, want %s in_progress held needs_input at P0", replayed, ref)
	}
	if projected, _ := tickets.Get(ctx, ref); projected.State != replayed.State || projected.Hold != replayed.Hold {
		t.Fatalf("projection %s/%s differs from replay %s/%s", projected.State, projected.Hold, replayed.State, replayed.Hold)
	}
}

func TestClearHoldCanInsistOnTheReason(t *testing.T) {
	t.Parallel()
	tickets, _ := newTickets(t, nil)
	bringTo(t, tickets, core.TicketReady)
	if _, err := tickets.SetHold(ctx, "system", ref, "attempts_exhausted"); err != nil {
		t.Fatal(err)
	}
	_, err := tickets.ClearHold(ctx, "human:alex", ref, "paused") // resume does not lift attempts_exhausted
	assertCode(t, err, core.CodeInvalidState)
	if got, err := tickets.ClearHold(ctx, "human:alex", ref, "attempts_exhausted"); err != nil || got.Hold != "" {
		t.Fatalf("ClearHold(attempts_exhausted) = %q, %v", got.Hold, err)
	}
}

func TestARecoveredTicketStartsInTheStateAndHoldTheTrackerHas(t *testing.T) {
	t.Parallel()
	tickets, s := newTickets(t, nil)
	tk, err := tickets.Recover(ctx, "human:alex", core.NewTicket{Ref: ref, Project: "web", Title: "Limits", BudgetOf: "gh:acme/web#1"}, core.TicketInProgress, "needs_input")
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if tk.State != core.TicketInProgress || tk.Hold != "needs_input" || tk.Title != "Limits" {
		t.Fatalf("recovered = %+v", tk)
	}
	events, _ := s.Read(ctx, "", store.Filter{Ticket: ref}, 0)
	replayed, err := core.ReplayTicket(events)
	if err != nil || replayed.State != core.TicketInProgress || replayed.Hold != "needs_input" || replayed.BudgetOf != "gh:acme/web#1" {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
	if _, err := tickets.Recover(ctx, "human:alex", core.NewTicket{Ref: ref}, core.TicketReady, ""); err == nil {
		t.Fatal("a ticket istari has was recovered over")
	}
	for _, bad := range []struct {
		state core.TicketState
		hold  string
	}{{"bogus", ""}, {core.TicketReady, "bogus"}} {
		if _, err := tickets.Recover(ctx, "human:alex", core.NewTicket{Ref: "gh:acme/web#77"}, bad.state, bad.hold); err == nil {
			t.Errorf("recovering into %q/%q was accepted", bad.state, bad.hold)
		}
	}
}

// endingTicket makes ticket gh:acme/web#1 with a completed, a waiting, a
// ready, a claimed and a blocked task, and a ready task of ticket #2.
func (f *fixture) endingTicket(t *testing.T, tickets *core.Tickets) {
	t.Helper()
	for _, ref := range []string{"gh:acme/web#1", "gh:acme/web#2"} {
		if _, err := tickets.Create(ctx, "human:alex", ref, "web"); err != nil {
			t.Fatal(err)
		}
	}
	f.create(t, "t_done", "implementer")
	f.complete(t, "t_done")
	f.create(t, "t_ready", "implementer")
	f.create(t, "t_waiting", "reviewer", "t_ready")
	f.create(t, "t_claimed", "implementer")
	f.claim(t, "t_claimed")
	f.create(t, "t_blocked", "implementer")
	if err := f.tasks.Block(ctx, f.claim(t, "t_blocked"), "needs_input"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{ID: "t_other", Ticket: "gh:acme/web#2", Project: "web", Role: "implementer"}); err != nil {
		t.Fatal(err)
	}
}

// assertEnded checks that the ticket's open tasks are canceled without a
// lease, its finished task is untouched and the other ticket's task is not.
func (f *fixture) assertEnded(t *testing.T) {
	t.Helper()
	want := map[string]core.TaskState{
		"t_done": core.TaskCompleted, "t_ready": core.TaskCanceled, "t_waiting": core.TaskCanceled,
		"t_claimed": core.TaskCanceled, "t_blocked": core.TaskCanceled, "t_other": core.TaskReady,
	}
	for id, state := range want {
		task, err := f.tasks.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State != state {
			t.Errorf("%s is %s, want %s", id, task.State, state)
		}
		if task.Owner != "" || !task.LeaseUntil.IsZero() {
			t.Errorf("%s still has a lease: %s until %s", id, task.Owner, task.LeaseUntil)
		}
	}
}

func TestCancelingATicketCancelsItsOpenTasksInTheSameEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	f.endingTicket(t, tickets)
	before := len(f.events(t, store.Filter{}))
	if _, err := tickets.Fire(ctx, "human:alex", "gh:acme/web#1", "cancel"); err != nil {
		t.Fatal(err)
	}
	if got := f.events(t, store.Filter{})[before:]; len(got) != 1 || got[0].Type != core.EventTicketTransitioned {
		t.Fatalf("cancel appended %d events (%v), want the one transition", len(got), got)
	}
	f.assertEnded(t)
}

func TestATicketThatIsMergedCancelsItsOpenTasks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	f.endingTicket(t, tickets)
	for _, trigger := range toDone {
		if _, err := tickets.Fire(ctx, "system", "gh:acme/web#1", trigger); err != nil {
			t.Fatalf("%s: %v", trigger, err)
		}
	}
	f.assertEnded(t)
}

func TestAHeldTicketThatIsMergedCancelsItsOpenTasks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	f.endingTicket(t, tickets)
	for _, trigger := range []string{"accept", "start", "plan_accepted", "pr_ready"} {
		if _, err := tickets.Fire(ctx, "system", "gh:acme/web#1", trigger); err != nil {
			t.Fatalf("%s: %v", trigger, err)
		}
	}
	if _, err := tickets.SetHold(ctx, "system", "gh:acme/web#1", "attempts_exhausted"); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Fire(ctx, "system", "gh:acme/web#1", "merged"); err != nil {
		t.Fatal(err)
	}
	f.assertEnded(t)
}
