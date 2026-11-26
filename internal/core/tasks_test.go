package core_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
)

var ctx = context.Background()

// clock is a manual clock for tests.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	store *store.SQLite
	clock *clock
	tasks *core.Tasks
}

func newFixture(t *testing.T) *fixture {
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
	c := &clock{now: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)}
	return &fixture{store: s, clock: c, tasks: core.NewTasks(s, core.TaskOptions{Clock: c.Now})}
}

func (f *fixture) create(t *testing.T, id, role string, needs ...string) core.Task {
	t.Helper()
	task, err := f.tasks.Create(ctx, "system", core.NewTask{
		ID: id, Ticket: "gh:acme/web#1", Project: "web", Role: role, Needs: needs,
	})
	if err != nil {
		t.Fatalf("Create %s: %v", id, err)
	}
	return task
}

func (f *fixture) state(t *testing.T, id string) core.TaskState {
	t.Helper()
	task, err := f.tasks.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return task.State
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *core.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestTwoConcurrentClaimsOfOneTaskExactlyOneWins(t *testing.T) {
	t.Parallel()
	for round := range 20 {
		f := newFixture(t)
		f.create(t, "t_1", "implementer")
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = f.tasks.Claim(ctx, "agent:implementer-"+string(rune('1'+i)), "t_1")
			}()
		}
		wg.Wait()
		won, lost := 0, 0
		for _, err := range errs {
			var e *core.Error
			switch {
			case err == nil:
				won++
			case errors.As(err, &e) && e.Code == core.CodeTaskClaimed:
				lost++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("round %d: %d claims won and %d got task_claimed, want 1 and 1", round, won, lost)
		}
	}
}

func (f *fixture) claim(t *testing.T, id string) core.Lease {
	t.Helper()
	lease, err := f.tasks.Claim(ctx, "agent:worker", id)
	if err != nil {
		t.Fatalf("Claim %s: %v", id, err)
	}
	return lease
}

func (f *fixture) complete(t *testing.T, id string) {
	t.Helper()
	if err := f.tasks.Complete(ctx, f.claim(t, id)); err != nil {
		t.Fatalf("Complete %s: %v", id, err)
	}
}

func TestATaskWithUnfinishedNeedsWaitsUntilAllAreCompleted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "plan", "planner")
	f.create(t, "verify", "verifier")
	if got := f.create(t, "review", "reviewer", "plan", "verify").State; got != core.TaskWaiting {
		t.Fatalf("review with unfinished needs is %s, want waiting", got)
	}
	f.complete(t, "plan")
	if got := f.state(t, "review"); got != core.TaskWaiting {
		t.Fatalf("review after one of two needs completed is %s, want waiting", got)
	}
	f.complete(t, "verify")
	if got := f.state(t, "review"); got != core.TaskReady {
		t.Fatalf("review after all needs completed is %s, want ready", got)
	}
	if got := f.create(t, "approve", "human", "plan").State; got != core.TaskReady {
		t.Fatalf("a task whose needs are already completed is %s, want ready", got)
	}
}

func TestAFailedOrCanceledPrerequisiteReadiesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "implement", "implementer")
	f.create(t, "plan", "planner")
	f.create(t, "after-implement", "reviewer", "implement")
	f.create(t, "after-plan", "reviewer", "plan")

	if err := f.tasks.Cancel(ctx, "human:alex", "implement"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	for range core.DefaultMaxAttempts {
		if err := f.tasks.Fail(ctx, f.claim(t, "plan"), "tests red"); err != nil {
			t.Fatalf("Fail: %v", err)
		}
	}
	if got := f.state(t, "plan"); got != core.TaskFailed {
		t.Fatalf("plan is %s, want failed", got)
	}
	for _, id := range []string{"after-implement", "after-plan"} {
		if got := f.state(t, id); got != core.TaskWaiting {
			t.Errorf("%s is %s, want waiting", id, got)
		}
	}
}

func TestInvalidNeedsAreRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "plan", "planner")
	for _, tc := range []struct {
		name  string
		id    string
		needs []string
	}{
		{"cycle through itself", "loop", []string{"plan", "loop"}},
		{"unknown task", "review", []string{"no-such-task"}},
	} {
		_, err := f.tasks.Create(ctx, "system", core.NewTask{
			ID: tc.id, Ticket: "gh:acme/web#1", Role: "reviewer", Needs: tc.needs,
		})
		assertCode(t, err, core.CodeInvalidNeeds)
		if _, err := f.tasks.Get(ctx, tc.id); err == nil {
			t.Errorf("%s: the task was created anyway", tc.name)
		}
	}
}

func (f *fixture) events(t *testing.T, filter store.Filter) []store.Event {
	t.Helper()
	events, err := f.store.Read(ctx, "", filter, 0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestAnExpiredLeaseReturnsTheTaskToReadyAndTheOldOwnerLosesIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_1", "implementer")
	old := f.claim(t, "t_1")

	f.clock.Advance(4 * time.Minute)
	renewed, err := f.tasks.Renew(ctx, old)
	if err != nil {
		t.Fatalf("Renew before the deadline: %v", err)
	}
	if want := time.Date(2026, 11, 24, 9, 9, 0, 0, time.UTC); !renewed.Until.Equal(want) { // renewed at 09:04 for 5 minutes
		t.Fatalf("renewed lease until %s, want %s", renewed.Until, want)
	}

	f.clock.Advance(core.DefaultLease - time.Second)
	if expired, err := f.tasks.ExpireLeases(ctx); err != nil || len(expired) != 0 {
		t.Fatalf("ExpireLeases before the deadline = %v, %v; want none", expired, err)
	}
	f.clock.Advance(time.Second)
	expired, err := f.tasks.ExpireLeases(ctx)
	if err != nil || len(expired) != 1 || expired[0] != "t_1" {
		t.Fatalf("ExpireLeases at the deadline = %v, %v; want [t_1]", expired, err)
	}
	if got := f.state(t, "t_1"); got != core.TaskReady {
		t.Fatalf("after expiry t_1 is %s, want ready", got)
	}
	if got := f.events(t, store.Filter{TypePrefix: "task.expired"}); len(got) != 1 || got[0].Task != "t_1" {
		t.Fatalf("task.expired events = %v, want one for t_1", got)
	}

	_, err = f.tasks.Renew(ctx, renewed)
	assertCode(t, err, core.CodeClaimLost)
	assertCode(t, f.tasks.Complete(ctx, renewed), core.CodeClaimLost)

	// The task can be claimed again, and the old lease still does not work.
	f.claim(t, "t_1")
	assertCode(t, f.tasks.Complete(ctx, renewed), core.CodeClaimLost)
}

func TestEveryOperationAppendsExactlyOneMatchingEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var lease core.Lease
	steps := []struct {
		want string
		op   func() error
	}{
		{"task.created", func() error {
			_, err := f.tasks.Create(ctx, "system", core.NewTask{ID: "t_1", Ticket: "gh:acme/web#1", Role: "implementer"})
			return err
		}},
		{"task.created", func() error {
			_, err := f.tasks.Create(ctx, "system", core.NewTask{ID: "t_2", Ticket: "gh:acme/web#1", Role: "reviewer", Needs: []string{"t_1"}})
			return err
		}},
		{"task.claimed", func() (err error) {
			lease, err = f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer")
			return err
		}},
		{"task.renewed", func() (err error) { lease, err = f.tasks.Renew(ctx, lease); return err }},
		{"task.released", func() error { return f.tasks.Release(ctx, lease) }},
		{"task.claimed", func() (err error) { lease, err = f.tasks.Claim(ctx, "agent:implementer-1", "t_1"); return err }},
		{"task.blocked", func() error { return f.tasks.Block(ctx, lease, "ci_red") }},
		{"task.unblocked", func() error { return f.tasks.Unblock(ctx, "human:alex", "t_1") }},
		{"task.claimed", func() (err error) {
			lease, err = f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer")
			return err
		}},
		{"task.failed", func() error { return f.tasks.Fail(ctx, lease, "tests red") }},
		{"task.claimed", func() (err error) {
			lease, err = f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer")
			return err
		}},
		{"task.expired", func() error { f.clock.Advance(core.DefaultLease); _, err := f.tasks.ExpireLeases(ctx); return err }},
		{"task.claimed", func() (err error) {
			lease, err = f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer")
			return err
		}},
		{"task.completed", func() error { return f.tasks.Complete(ctx, lease) }},
		{"task.canceled", func() error { return f.tasks.Cancel(ctx, "human:alex", "t_2") }},
	}
	seen := 0
	for i, step := range steps {
		if err := step.op(); err != nil {
			t.Fatalf("step %d (%s): %v", i, step.want, err)
		}
		events := f.events(t, store.Filter{})
		if len(events) != seen+1 {
			t.Fatalf("step %d (%s) appended %d events, want 1", i, step.want, len(events)-seen)
		}
		if got := events[seen].Type; got != step.want {
			t.Fatalf("step %d appended %s, want %s", i, got, step.want)
		}
		seen++
	}
	if got := f.state(t, "t_1"); got != core.TaskCompleted {
		t.Errorf("t_1 ends %s, want completed", got)
	}
}

func TestBlockNeedsAKnownKindAndUnblockReturnsToReady(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_1", "implementer")
	lease := f.claim(t, "t_1")
	if err := f.tasks.Block(ctx, lease, "bored"); err == nil {
		t.Fatal("Block accepted an unknown kind")
	}
	for _, kind := range []string{"needs_input", "awaiting_approval", "budget_exceeded", "ci_red"} {
		if err := f.tasks.Block(ctx, lease, kind); err != nil {
			t.Fatalf("Block %s: %v", kind, err)
		}
		if task, _ := f.tasks.Get(ctx, "t_1"); task.State != core.TaskBlocked || task.BlockKind != kind {
			t.Fatalf("after Block %s: %s/%s", kind, task.State, task.BlockKind)
		}
		if err := f.tasks.Unblock(ctx, "human:alex", "t_1"); err != nil {
			t.Fatal(err)
		}
		if got := f.state(t, "t_1"); got != core.TaskReady {
			t.Fatalf("after Unblock: %s, want ready", got)
		}
		lease = f.claim(t, "t_1")
	}
	assertCode(t, f.tasks.Unblock(ctx, "human:alex", "t_1"), core.CodeInvalidState)
}

func TestClaimReadyTakesTheOldestReadyTaskOfTheRole(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_b", "implementer")
	f.clock.Advance(time.Second)
	f.create(t, "t_a", "implementer")
	f.create(t, "t_r", "reviewer")
	lease, err := f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer")
	if err != nil || lease.Task != "t_b" {
		t.Fatalf("first ClaimReady = %s, %v; want t_b", lease.Task, err)
	}
	if lease, err = f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer"); err != nil || lease.Task != "t_a" {
		t.Fatalf("second ClaimReady = %s, %v; want t_a", lease.Task, err)
	}
	_, err = f.tasks.ClaimReady(ctx, "agent:implementer-1", "implementer")
	assertCode(t, err, core.CodeNoReadyTask)
}

func TestExhaustingAttemptsLeavesTheTaskFailed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_1", "implementer")
	for attempt := 1; attempt <= core.DefaultMaxAttempts; attempt++ {
		if err := f.tasks.Fail(ctx, f.claim(t, "t_1"), "tests red"); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	task, _ := f.tasks.Get(ctx, "t_1")
	if task.State != core.TaskFailed || task.Attempts != 3 {
		t.Fatalf("after 3 failures: %s with %d attempts, want failed with 3", task.State, task.Attempts)
	}
	if got := f.events(t, store.Filter{TypePrefix: "task.failed"}); len(got) != 2 {
		t.Errorf("task.failed events = %d, want 2", len(got))
	}
	exhausted := f.events(t, store.Filter{TypePrefix: "task.attempts_exhausted"})
	if len(exhausted) != 1 || exhausted[0].Ticket != "gh:acme/web#1" {
		t.Fatalf("task.attempts_exhausted events = %v, want one carrying the ticket", exhausted)
	}
	_, err := f.tasks.Claim(ctx, "agent:worker", "t_1")
	assertCode(t, err, core.CodeInvalidState)
}

func TestAnOwnerPastItsDeadlineLosesTheClaimBeforeExpiryRuns(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_1", "implementer")
	lease := f.claim(t, "t_1")
	f.clock.Advance(core.DefaultLease)
	_, err := f.tasks.Renew(ctx, lease)
	assertCode(t, err, core.CodeClaimLost)
	assertCode(t, f.tasks.Complete(ctx, lease), core.CodeClaimLost)
}

// slowTasks returns a task machine whose clock sleeps, which widens the window
// between reading records and committing, so races show up reliably.
func slowTasks(t *testing.T) *core.Tasks {
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
	slow := func() time.Time {
		time.Sleep(20 * time.Millisecond)
		return time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	}
	return core.NewTasks(s, core.TaskOptions{Clock: slow, Lease: time.Hour})
}

func TestATaskCreatedWhileItsNeedCompletesIsNotLeftWaiting(t *testing.T) {
	t.Parallel()
	tasks := slowTasks(t)
	if _, err := tasks.Create(ctx, "system", core.NewTask{ID: "plan", Ticket: "gh:acme/web#1", Role: "planner"}); err != nil {
		t.Fatal(err)
	}
	lease, err := tasks.Claim(ctx, "agent:1", "plan")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := tasks.Complete(ctx, lease); err != nil {
			t.Errorf("Complete: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := tasks.Create(ctx, "system", core.NewTask{ID: "review", Ticket: "gh:acme/web#1", Role: "reviewer", Needs: []string{"plan"}}); err != nil {
			t.Errorf("Create: %v", err)
		}
	}()
	wg.Wait()
	if review, _ := tasks.Get(ctx, "review"); review.State != core.TaskReady {
		t.Fatalf("review is %s after its need completed, want ready", review.State)
	}
}

func TestATaskWhoseLastNeedsCompleteTogetherBecomesReady(t *testing.T) {
	t.Parallel()
	tasks := slowTasks(t)
	for _, nt := range []core.NewTask{
		{ID: "a", Ticket: "gh:acme/web#1", Role: "implementer"},
		{ID: "b", Ticket: "gh:acme/web#1", Role: "verifier"},
		{ID: "d", Ticket: "gh:acme/web#1", Role: "reviewer", Needs: []string{"a", "b"}},
	} {
		if _, err := tasks.Create(ctx, "system", nt); err != nil {
			t.Fatal(err)
		}
	}
	leaseA, errA := tasks.Claim(ctx, "agent:1", "a")
	leaseB, errB := tasks.Claim(ctx, "agent:2", "b")
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	var wg sync.WaitGroup
	for _, lease := range []core.Lease{leaseA, leaseB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tasks.Complete(ctx, lease); err != nil {
				t.Errorf("Complete %s: %v", lease.Task, err)
			}
		}()
	}
	wg.Wait()
	if d, _ := tasks.Get(ctx, "d"); d.State != core.TaskReady {
		t.Fatalf("d is %s after both needs completed, want ready", d.State)
	}
}

func TestDuplicateNeedsAreRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "plan", "planner")
	_, err := f.tasks.Create(ctx, "system", core.NewTask{ID: "review", Ticket: "gh:acme/web#1", Role: "reviewer", Needs: []string{"plan", "plan"}})
	assertCode(t, err, core.CodeInvalidNeeds)
}

func TestClaimReadyWithoutARoleTakesTheOldestReadyTaskOfAnyRole(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_r", "reviewer")
	f.clock.Advance(time.Second)
	f.create(t, "t_i", "implementer")
	lease, err := f.tasks.ClaimReady(ctx, "agent:any", "")
	if err != nil || lease.Task != "t_r" {
		t.Fatalf("ClaimReady(any role) = %s, %v; want t_r", lease.Task, err)
	}
}

func TestLeaseOfReturnsTheCurrentLeaseToItsOwnerOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_1", "implementer")
	lease := f.claim(t, "t_1")
	got, err := f.tasks.LeaseOf(ctx, "t_1", lease.Owner)
	if err != nil || got != lease {
		t.Fatalf("LeaseOf(owner) = %+v, %v; want %+v", got, err, lease)
	}
	_, err = f.tasks.LeaseOf(ctx, "t_1", "agent:someone-else")
	assertCode(t, err, core.CodeClaimLost)
}

func TestRetryReturnsAFailedTaskToReadyWithFreshAttempts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_1", "implementer")
	assertCode(t, f.tasks.Retry(ctx, "human:alex", "t_1"), core.CodeInvalidState) // not failed
	for range core.DefaultMaxAttempts {
		if err := f.tasks.Fail(ctx, f.claim(t, "t_1"), "red"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.tasks.Retry(ctx, "human:alex", "t_1"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	task, _ := f.tasks.Get(ctx, "t_1")
	if task.State != core.TaskReady || task.Attempts != 0 {
		t.Fatalf("after Retry: %s with %d attempts", task.State, task.Attempts)
	}
	if got := f.events(t, store.Filter{TypePrefix: "task.retried"}); len(got) != 1 {
		t.Fatalf("task.retried events = %d, want 1", len(got))
	}
}

func TestRevokingALeaseReturnsTheTaskAtOnceAndTheOldLeaseIsDead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "t_1", "implementer")
	assertCode(t, f.tasks.Revoke(ctx, "human:alex", "t_1"), core.CodeInvalidState) // not claimed
	lease := f.claim(t, "t_1")
	if err := f.tasks.Revoke(ctx, "human:alex", "t_1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if task, _ := f.tasks.Get(ctx, "t_1"); task.State != core.TaskReady || task.Owner != "" {
		t.Fatalf("after Revoke: %s owned by %q", task.State, task.Owner)
	}
	if err := f.tasks.Complete(ctx, lease); err == nil {
		t.Fatal("the revoked lease still completes the task")
	}
}

// staleTask makes a store as it was before a finished ticket canceled its
// tasks: ticket gh:acme/web#1 is ended by trigger, its task t_1 is ready.
func (f *fixture) staleTask(t *testing.T, triggers ...string) *core.Tickets {
	t.Helper()
	tickets := core.NewTickets(f.store, core.TicketOptions{Clock: f.clock.Now})
	if _, err := tickets.Create(ctx, "human:alex", "gh:acme/web#1", "web"); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range triggers {
		if _, err := tickets.Fire(ctx, "system", "gh:acme/web#1", trigger); err != nil {
			t.Fatalf("%s: %v", trigger, err)
		}
	}
	f.create(t, "t_1", "implementer")
	return tickets
}

var toDone = []string{"accept", "start", "plan_accepted", "pr_ready", "review_passed", "merged"}

func TestATaskOfAFinishedTicketIsHandedOutToNobody(t *testing.T) {
	t.Parallel()
	for name, triggers := range map[string][]string{"canceled": {"cancel"}, "done": toDone} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.staleTask(t, triggers...)
			_, err := f.tasks.ClaimReady(ctx, "agent:a", "implementer")
			assertCode(t, err, core.CodeNoReadyTask)
			_, err = f.tasks.ClaimReadyIn(ctx, "agent:a", "", []string{"web"})
			assertCode(t, err, core.CodeNoReadyTask)
			_, err = f.tasks.Claim(ctx, "agent:a", "t_1")
			assertCode(t, err, core.CodeInvalidState)
		})
	}
}

func TestCancelOfFinishedCancelsTheOpenTasksOfFinishedTicketsOnce(t *testing.T) {
	t.Parallel()
	for name, triggers := range map[string][]string{"canceled": {"cancel"}, "done": toDone} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tickets := f.staleTask(t, triggers...)
			if _, err := tickets.Create(ctx, "human:alex", "gh:acme/web#2", "web"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tasks.Create(ctx, "system", core.NewTask{ID: "t_2", Ticket: "gh:acme/web#2", Project: "web", Role: "implementer"}); err != nil {
				t.Fatal(err)
			}
			canceled, err := f.tasks.CancelOfFinished(ctx, "system")
			if err != nil || len(canceled) != 1 || canceled[0] != "t_1" {
				t.Fatalf("CancelOfFinished = %v, %v; want [t_1]", canceled, err)
			}
			if got := f.state(t, "t_1"); got != core.TaskCanceled {
				t.Errorf("t_1 is %s, want canceled", got)
			}
			if got := f.state(t, "t_2"); got != core.TaskReady {
				t.Errorf("t_2 of the open ticket is %s, want ready", got)
			}
			before := len(f.events(t, store.Filter{}))
			if canceled, err := f.tasks.CancelOfFinished(ctx, "system"); err != nil || len(canceled) != 0 {
				t.Fatalf("second CancelOfFinished = %v, %v; want nothing", canceled, err)
			}
			if after := len(f.events(t, store.Filter{})); after != before {
				t.Fatalf("the second call appended %d events", after-before)
			}
		})
	}
}
