package runner_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
)

// fakeHarness records what it was started with; each session ends when the
// test says so, or when it is stopped.
type fakeHarness struct {
	name    string
	mu      sync.Mutex
	started chan fakeSession
	events  []harness.Event
}

type fakeSession struct {
	spec   harness.RunSpec
	finish chan harness.Result // the test ends the run with this result
	out    chan harness.Event
	stop   chan struct{}
	once   *sync.Once
}

func newFakeHarness(name string, events ...harness.Event) *fakeHarness {
	h := &fakeHarness{name: name, started: make(chan fakeSession, 8), events: events}
	harness.Register(h)
	return h
}

func (h *fakeHarness) Name() string                { return h.name }
func (h *fakeHarness) Check(context.Context) error { return nil }
func (h *fakeHarness) Start(_ context.Context, spec harness.RunSpec) (harness.Session, error) {
	s := fakeSession{spec: spec, finish: make(chan harness.Result, 1), out: make(chan harness.Event, len(h.events)), stop: make(chan struct{}), once: &sync.Once{}}
	for _, e := range h.events {
		s.out <- e
	}
	close(s.out)
	h.started <- s
	return s, nil
}

func (s fakeSession) ID() string                   { return "fake" }
func (s fakeSession) Events() <-chan harness.Event { return s.out }
func (s fakeSession) Send(string) error            { return harness.ErrSendNotSupported }
func (s fakeSession) Stop(time.Duration) error     { s.once.Do(func() { close(s.stop) }); return nil }
func (s fakeSession) Wait() (harness.Result, error) {
	select {
	case r := <-s.finish:
		return r, nil
	case <-s.stop:
		return harness.Result{ExitCode: -1}, nil
	}
}

type runFixture struct {
	*wsFixture
	tasks  *core.Tasks
	runner *runner.Runner
	h      *fakeHarness
}

var fakes sync.Map

func newRunner(t *testing.T, opts runner.RunnerOptions, events ...harness.Event) *runFixture {
	t.Helper()
	f := newWorkspaces(t)
	name := "fake-" + t.Name()
	var h *fakeHarness
	if v, ok := fakes.Load(name); ok {
		h = v.(*fakeHarness)
	} else {
		h = newFakeHarness(name, events...)
		fakes.Store(name, h)
	}
	if opts.Lease == 0 {
		opts.Lease = time.Minute
	}
	tasks := core.NewTasks(f.store, core.TaskOptions{Lease: opts.Lease})
	opts.DataDir = f.data
	opts.Projects = []string{"web"}
	opts.Slots = []runner.Slot{{Profile: "implementer-1", Role: "implementer", Harness: harness.Profile{Harness: name}}}
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + f.root + "/remotes"})
	r := runner.New(f.store, tasks, f.tickets, ws, opts)
	return &runFixture{wsFixture: f, tasks: tasks, runner: r, h: h}
}

// readyTask makes ticket #n in planning with a ready implementer task.
func (f *runFixture) readyTask(t *testing.T, n int) (core.Ticket, core.Task) {
	t.Helper()
	tk := f.ticket(t, n, "Run me")
	f.fire(t, tk.Ref, "accept", "start")
	task, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: tk.Ref, Project: "web", Role: "implementer"})
	if err != nil {
		t.Fatal(err)
	}
	return tk, task
}

func (f *runFixture) start(t *testing.T) context.CancelFunc {
	t.Helper()
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.runner.Run(c); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func (f *runFixture) session(t *testing.T) fakeSession {
	t.Helper()
	select {
	case s := <-f.h.started:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("the harness was not started")
		return fakeSession{}
	}
}

func (f *runFixture) waitTask(t *testing.T, id string, want core.TaskState) core.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		task, err := f.tasks.Get(ctx, id)
		if err == nil && task.State == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s is %s, want %s", id, task.State, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitEvent waits for the first event of typ and returns it.
func (f *runFixture) waitEvent(t *testing.T, typ string) store.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if events := f.events(t, typ); len(events) > 0 {
			return events[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event", typ)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestASlotClaimsAReadyTaskAndStartsTheHarnessInTheTicketWorktree(t *testing.T) {
	f := newRunner(t, runner.RunnerOptions{})
	tk, task := f.readyTask(t, 1)
	f.start(t)
	s := f.session(t)
	if s.spec.Workdir != f.dir(1) {
		t.Fatalf("workdir = %s, want the ticket worktree %s", s.spec.Workdir, f.dir(1))
	}
	for key, want := range map[string]string{"ISTARI_PROFILE": "implementer-1", "ISTARI_ROLE": "implementer", "ISTARI_TICKET": tk.Ref, "ISTARI_TASK": task.ID} {
		if got := s.spec.Env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if s.spec.Env["ISTARI_TOKEN"] == "" {
		t.Error("ISTARI_TOKEN is empty")
	}
	if claimed := f.waitTask(t, task.ID, core.TaskClaimed); claimed.Owner != "agent:implementer-1" {
		t.Fatalf("owner = %s, want agent:implementer-1", claimed.Owner)
	}
	if _, err := f.runner.Runs().Complete(ctx, s.spec.Env["ISTARI_RUN"], "done"); err != nil {
		t.Fatalf("istari complete: %v", err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	f.waitTask(t, task.ID, core.TaskCompleted)
}

func TestARunPastItsTimeoutIsStoppedAndTheTaskFailsWithTheReason(t *testing.T) {
	f := newRunner(t, runner.RunnerOptions{Timeout: 300 * time.Millisecond})
	_, task := f.readyTask(t, 2)
	f.start(t)
	f.session(t) // never finishes by itself
	failed := f.waitEvent(t, "task.failed")
	if failed.Ticket == "" || !strings.Contains(string(failed.Payload), "timeout") {
		t.Fatalf("task.failed = %s, want the timeout as the reason", failed.Payload)
	}
	run, err := f.runner.Runs().Get(ctx, runOf(t, f.waitEvent(t, "run.exited")))
	if err != nil || !strings.Contains(run.Reason, "timeout") || run.State != runner.RunEnded || run.Task != task.ID {
		t.Fatalf("run = %+v, %v; want an ended run with a timeout reason", run, err)
	}
}

func TestARunThatSpendsItsBudgetBlocksTheTask(t *testing.T) {
	f := newRunner(t, runner.RunnerOptions{BudgetUSD: 1},
		harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 0.6}},
		harness.Event{Kind: harness.EventUsage, Usage: harness.Usage{CostUSD: 0.6}})
	_, task := f.readyTask(t, 3)
	f.start(t)
	f.session(t)
	if blocked := f.waitTask(t, task.ID, core.TaskBlocked); blocked.BlockKind != "budget_exceeded" {
		t.Fatalf("block kind = %s, want budget_exceeded", blocked.BlockKind)
	}
}

func TestAnExitWithoutIstariCompleteFailsTheTask(t *testing.T) {
	f := newRunner(t, runner.RunnerOptions{})
	_, task := f.readyTask(t, 4)
	f.start(t)
	s := f.session(t)
	s.finish <- harness.Result{ExitCode: 0}
	if failed := f.waitEvent(t, "task.failed"); !strings.Contains(string(failed.Payload), "without istari complete") || failed.Ticket != task.Ticket {
		t.Fatalf("task.failed = %s", failed.Payload)
	}
}

func TestTheLeaseIsRenewedWhileTheRunLivesAndTheTokenDiesWithIt(t *testing.T) {
	f := newRunner(t, runner.RunnerOptions{Lease: 600 * time.Millisecond})
	_, task := f.readyTask(t, 5)
	f.start(t)
	s := f.session(t)
	token := s.spec.Env["ISTARI_TOKEN"]
	time.Sleep(1500 * time.Millisecond) // two and a half leases
	if got := f.waitTask(t, task.ID, core.TaskClaimed); !got.LeaseUntil.After(time.Now()) {
		t.Fatalf("the lease ran out while the run lived: until %s", got.LeaseUntil)
	}
	if _, err := f.runner.Runs().ByToken(ctx, token); err != nil {
		t.Fatalf("the token of a living run: %v", err)
	}
	s.finish <- harness.Result{ExitCode: 1}
	f.waitEvent(t, "task.failed")
	if _, err := f.runner.Runs().ByToken(ctx, token); err == nil {
		t.Fatal("the token still works after its run ended")
	}
}

func runOf(t *testing.T, e store.Event) string {
	t.Helper()
	var p struct{ Run string }
	if err := json.Unmarshal(e.Payload, &p); err != nil || p.Run == "" {
		t.Fatalf("event %s names no run: %s", e.Type, e.Payload)
	}
	return p.Run
}
