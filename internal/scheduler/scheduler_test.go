package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/scheduler"
	"github.com/sbraznikov/istari/internal/store"
)

var ctx = context.Background()

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// daemon is the scheduler wired the way the daemon wires it, on a database
// that survives "restarts" of the daemon within a test.
type daemon struct {
	store *store.SQLite
	srv   *api.Server
	sched *scheduler.Scheduler
}

func start(t *testing.T, path string, c *clock, exec func(scheduler.Exec) scheduler.Exec) *daemon {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.NewServer(s, api.ServerOptions{Clock: c.Now})
	run := scheduler.Exec(srv.Do)
	if exec != nil {
		run = exec(run)
	}
	d := &daemon{store: s, srv: srv, sched: scheduler.New(srv.Store(), srv.Tasks(), run, scheduler.Options{Clock: c.Now, Prepare: srv.Prepare})}
	t.Cleanup(func() { s.Close() })
	return d
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)} }

func (d *daemon) count(t *testing.T, filter store.Filter) int {
	t.Helper()
	events, err := d.store.Read(ctx, "", filter, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

func TestALeaseThatIsNotRenewedExpiresWithinOneTickAfterItsDeadline(t *testing.T) {
	c := newClock()
	d := start(t, filepath.Join(t.TempDir(), "istari.db"), c, nil)
	if _, err := d.srv.Tasks().Create(ctx, "system", core.NewTask{ID: "t_1", Ticket: "gh:acme/web#1", Role: "implementer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.Tasks().Claim(ctx, "agent:a", "t_1"); err != nil {
		t.Fatal(err)
	}
	c.Advance(core.DefaultLease - time.Second)
	if err := d.sched.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if task, _ := d.srv.Tasks().Get(ctx, "t_1"); task.State != core.TaskClaimed {
		t.Fatalf("before the deadline t_1 is %s", task.State)
	}
	c.Advance(time.Second) // the deadline; the next tick expires it
	if err := d.sched.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if task, _ := d.srv.Tasks().Get(ctx, "t_1"); task.State != core.TaskReady {
		t.Fatalf("one tick after the deadline t_1 is %s, want ready", task.State)
	}
}

func cancelRequest(ref string) json.RawMessage {
	return json.RawMessage(`{"actor":"human:alex","ref":"` + ref + `","trigger":"cancel"}`)
}

func TestAScheduledActionFiresOnceAtItsTimeAndSurvivesARestart(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "istari.db")
	d := start(t, path, c, nil)
	ref := "gh:acme/web#1"
	if _, err := d.srv.Tickets().Create(ctx, "human:alex", ref, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sched.Schedule(ctx, "human:alex", c.Now().Add(10*time.Minute), "ticket.fire", cancelRequest(ref)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	c.Advance(5 * time.Minute)
	if err := d.sched.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if tk, _ := d.srv.Tickets().Get(ctx, ref); tk.State != core.TicketNew {
		t.Fatalf("the action fired early: %s", tk.State)
	}
	d.store.Close()

	d = start(t, path, c, nil) // a restarted daemon
	c.Advance(5 * time.Minute)
	for range 3 {
		if err := d.sched.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if tk, _ := d.srv.Tickets().Get(ctx, ref); tk.State != core.TicketCanceled {
		t.Fatalf("after its time the ticket is %s, want canceled", tk.State)
	}
	if n := d.count(t, store.Filter{TypePrefix: "timer.fired"}); n != 1 {
		t.Fatalf("timer.fired events = %d, want 1", n)
	}
}

func TestAnInterruptionBetweenTheActionAndItsRecordDoesNotRepeatTheAction(t *testing.T) {
	c := newClock()
	calls := 0
	// The first run performs the action, then fails as if the daemon died
	// before recording that the timer fired.
	crashOnce := func(next scheduler.Exec) scheduler.Exec {
		return func(ctx context.Context, op, key string, req json.RawMessage) error {
			calls++
			err := next(ctx, op, key, req)
			if calls == 1 {
				return errors.New("daemon died")
			}
			return err
		}
	}
	d := start(t, filepath.Join(t.TempDir(), "istari.db"), c, crashOnce)
	// Creating a task without an id is not refused when repeated: only the
	// idempotency key keeps a second run from creating a second task.
	create := json.RawMessage(`{"actor":"system","ticket":"gh:acme/web#1","role":"implementer"}`)
	if _, err := d.sched.Schedule(ctx, "human:alex", c.Now(), "task.create", create); err != nil {
		t.Fatal(err)
	}
	d.sched.Step(ctx) // runs the action, "dies"
	if err := d.sched.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("the action ran %d times, want a retry after the interruption", calls)
	}
	if tasks, _ := d.srv.Tasks().List(ctx); len(tasks) != 1 {
		t.Fatalf("%d tasks exist, want 1: the create happened once", len(tasks))
	}
}

func TestARefusedActionFiresOnceAndRecordsTheRefusal(t *testing.T) {
	c := newClock()
	d := start(t, filepath.Join(t.TempDir(), "istari.db"), c, nil)
	timer, err := d.sched.Schedule(ctx, "human:alex", c.Now(), "ticket.fire", cancelRequest("gh:acme/web#404"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.sched.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	timers, _ := d.sched.List(ctx)
	if len(timers) != 1 || timers[0].ID != timer.ID || timers[0].State != scheduler.TimerFired || timers[0].Result == "" {
		t.Fatalf("timers = %+v, want one fired timer with the refusal as its result", timers)
	}
	if n := d.count(t, store.Filter{TypePrefix: "timer.fired"}); n != 1 {
		t.Fatalf("timer.fired events = %d, want 1", n)
	}
}

func TestScheduleRejectsAnUnknownOperationOrAReadOperation(t *testing.T) {
	c := newClock()
	d := start(t, filepath.Join(t.TempDir(), "istari.db"), c, nil)
	for _, op := range []string{"task.explode", "task.list"} {
		if _, err := d.sched.Schedule(ctx, "human:alex", c.Now(), op, json.RawMessage(`{}`)); err == nil {
			t.Errorf("Schedule(%s) succeeded", op)
		}
	}
}

func TestAnActionRefusedForNowIsTriedAgainOnTheNextTick(t *testing.T) {
	c := newClock()
	d := start(t, filepath.Join(t.TempDir(), "istari.db"), c, nil)
	if _, err := d.srv.Tasks().Create(ctx, "system", core.NewTask{ID: "t_1", Ticket: "gh:acme/web#1", Role: "implementer"}); err != nil {
		t.Fatal(err)
	}
	if err := d.srv.Tasks().Pause(ctx, "human:alex", ""); err != nil {
		t.Fatal(err)
	}
	claim := json.RawMessage(`{"actor":"agent:a","id":"t_1"}`)
	if _, err := d.sched.Schedule(ctx, "human:alex", c.Now(), "task.claim", claim); err != nil {
		t.Fatal(err)
	}
	d.sched.Step(ctx) // paused: not yet
	if timers, _ := d.sched.List(ctx); timers[0].State != scheduler.TimerPending {
		t.Fatalf("a paused claim marked the timer %s", timers[0].State)
	}
	d.srv.Tasks().Resume(ctx, "human:alex", "")
	d.sched.Step(ctx)
	if task, _ := d.srv.Tasks().Get(ctx, "t_1"); task.State != core.TaskClaimed {
		t.Fatalf("after resume the scheduled claim left t_1 %s", task.State)
	}
}

func TestScheduleChecksTheRequestAndFillsTheActor(t *testing.T) {
	c := newClock()
	d := start(t, filepath.Join(t.TempDir(), "istari.db"), c, nil)
	if _, err := d.sched.Schedule(ctx, "human:alex", c.Now(), "ticket.fire", json.RawMessage(`{"ref":"x","colour":"red"}`)); err == nil {
		t.Error("Schedule accepted a request with an unknown field")
	}
	// No actor in the request: the one who schedules acts.
	timer, err := d.sched.Schedule(ctx, "human:alex", c.Now(), "ticket.create", json.RawMessage(`{"ref":"gh:acme/web#1"}`))
	if err != nil {
		t.Fatal(err)
	}
	d.sched.Step(ctx)
	events, _ := d.store.Read(ctx, "", store.Filter{Ticket: "gh:acme/web#1"}, 0)
	if len(events) != 1 || events[0].Actor != "human:alex" {
		t.Fatalf("timer %s created %v, want one event by human:alex", timer.ID, events)
	}
	// An operation whose request has no actor field gets none added.
	if _, err := d.sched.Schedule(ctx, "human:alex", c.Now(), "task.release", json.RawMessage(`{"lease":{"task":"t","owner":"agent:a","token":"x"}}`)); err != nil {
		t.Fatalf("Schedule task.release: %v", err)
	}
}
