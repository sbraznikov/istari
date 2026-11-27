package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/harness/claudecode"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// fakeHarness records what it was started with; each session ends when the
// test says so, or when it is stopped.
type fakeHarness struct {
	name    string
	mu      sync.Mutex
	started chan fakeSession
	events  []harness.Event
	fail    error // Start returns it while set
}

// failStart makes Start return err until it is called with nil.
func (h *fakeHarness) failStart(err error) { h.mu.Lock(); h.fail = err; h.mu.Unlock() }

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
	h.mu.Lock()
	fail := h.fail
	h.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
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

// fakeCount makes every fixture's harness name unique, so no two fixtures
// (parallel tests, or one test run again by -count) share a fake harness.
var fakeCount atomic.Int64

func newRunner(t *testing.T, opts runner.RunnerOptions, events ...harness.Event) *runFixture {
	t.Helper()
	f := newWorkspaces(t)
	h := newFakeHarness("fake-"+t.Name()+"-"+strconv.FormatInt(fakeCount.Add(1), 10), events...)
	name := h.name
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

// waitFor bounds how long a test waits for the runner: preparing, stopping
// and checkpointing a run spawn git, and under -race and parallel load that
// takes over 10s.
const waitFor = 30 * time.Second

func (f *runFixture) session(t *testing.T) fakeSession {
	t.Helper()
	select {
	case s := <-f.h.started:
		return s
	case <-time.After(waitFor):
		t.Fatal("the harness was not started")
		return fakeSession{}
	}
}

func (f *runFixture) waitTask(t *testing.T, id string, want core.TaskState) core.Task {
	t.Helper()
	return f.waitTaskWithin(t, id, want, waitFor)
}

// waitTaskWithin waits up to d for the task to be in state want.
func (f *runFixture) waitTaskWithin(t *testing.T, id string, want core.TaskState, d time.Duration) core.Task {
	t.Helper()
	deadline := time.Now().Add(d)
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
	deadline := time.Now().Add(waitFor)
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
	t.Parallel()
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
	if !strings.Contains(s.spec.Instructions, "Refactoring is not part of the cycle") {
		t.Errorf("instructions = %q, want the implementer's", s.spec.Instructions)
	}
	if !strings.Contains(s.spec.Brief, `<untrusted source="ticket `+tk.Ref+`">`) || !strings.Contains(s.spec.Brief, "Run me") {
		t.Errorf("brief = %q, want the ticket, delimited", s.spec.Brief)
	}
	if !exists(filepath.Join(s.spec.Workdir, ".istari", "skill.md")) {
		t.Error("the agent skill is not installed in the workspace")
	}
	if status := strings.TrimSpace(gitIn(t, s.spec.Workdir, "status", "--porcelain")); status != "" {
		t.Errorf("installing the skill left the worktree dirty: %q", status)
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
	t.Parallel()
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
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Budgets: map[string]float64{"M": 1}}, // a ticket without a size has the M budget
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
	t.Parallel()
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
	t.Parallel()
	const lease = 1500 * time.Millisecond
	f := newRunner(t, runner.RunnerOptions{Lease: lease})
	_, task := f.readyTask(t, 5)
	f.start(t)
	s := f.session(t)
	token := s.spec.Env["ISTARI_TOKEN"]
	f.waitRenewedPastTheClaim(t, lease)
	if got := f.waitTask(t, task.ID, core.TaskClaimed); got.Owner != "agent:implementer-1" {
		t.Fatalf("the living run's task is held by %s, want agent:implementer-1", got.Owner)
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

func TestStoppingARunFailsItsTaskWithWhoStoppedIt(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	f.readyTask(t, 7)
	f.start(t)
	s := f.session(t)
	if err := f.runner.Stop(ctx, s.spec.Env["ISTARI_RUN"], "human:alex"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if failed := f.waitEvent(t, "task.failed"); !strings.Contains(string(failed.Payload), "stopped by human:alex") {
		t.Fatalf("task.failed = %s", failed.Payload)
	}
}

// leftRunning gives a store holding one ended run and one run still recorded
// running, and a second runner on that store, as a restarted daemon has.
func leftRunning(t *testing.T, n int) (f *runFixture, restarted *runner.Runner, ended, running string) {
	t.Helper()
	f = newRunner(t, runner.RunnerOptions{})
	f.readyTask(t, n)
	f.readyTask(t, n+1)
	f.start(t)
	first := f.session(t)
	ended = first.spec.Env["ISTARI_RUN"]
	first.finish <- harness.Result{ExitCode: 1}
	running = f.session(t).spec.Env["ISTARI_RUN"]
	restarted = runner.New(f.store, f.tasks, f.tickets, runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data}), runner.RunnerOptions{DataDir: f.data})
	return f, restarted, ended, running
}

func exitsOf(t *testing.T, f *runFixture, run string) int {
	t.Helper()
	n := 0
	for _, e := range f.events(t, runner.EventRunExited) {
		if runOf(t, e) == run {
			n++
		}
	}
	return n
}

func TestEndingRunsLeftRunningEndsEachWithTheReasonInOneEvent(t *testing.T) {
	t.Parallel()
	f, restarted, ended, running := leftRunning(t, 9)
	before, err := restarted.Runs().Get(ctx, ended)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Runs().EndLeftRunning(ctx, "the daemon restarted"); err != nil {
		t.Fatalf("EndLeftRunning: %v", err)
	}
	got, err := restarted.Runs().Get(ctx, running)
	if err != nil || got.State != runner.RunEnded || got.Reason != "the daemon restarted" || got.Ended.IsZero() {
		t.Fatalf("run left running = %+v, %v; want ended with the reason and an end time", got, err)
	}
	if n := exitsOf(t, f, running); n != 1 {
		t.Errorf("run.exited events of the run left running = %d, want 1", n)
	}
	after, err := restarted.Runs().Get(ctx, ended)
	if err != nil || after.Reason != before.Reason || !after.Ended.Equal(before.Ended) {
		t.Errorf("the ended run changed: %+v, want %+v", after, before)
	}
	if n := exitsOf(t, f, ended); n != 1 {
		t.Errorf("run.exited events of the ended run = %d, want its own 1", n)
	}
}

func TestStoppingAnEndedRunSaysItHasEndedWithItsReason(t *testing.T) {
	t.Parallel()
	_, restarted, _, running := leftRunning(t, 11)
	if err := restarted.Runs().EndLeftRunning(ctx, "the daemon restarted"); err != nil {
		t.Fatal(err)
	}
	err := restarted.Stop(ctx, running, "human:alex")
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeInvalidState || e.Message != "run "+running+" has ended: the daemon restarted" {
		t.Fatalf("Stop = %v, want invalid_state saying the run has ended with its reason", err)
	}
}

func TestTheRefinerReadsTheMainBranchWithoutATicketWorktree(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	tk := f.ticket(t, 8, "Raw idea") // new: before planning
	f.runner = runner.New(f.store, f.tasks, f.tickets, runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")}),
		runner.RunnerOptions{DataDir: f.data, Projects: []string{"web"}, Slots: []runner.Slot{{Profile: "refiner-1", Role: "refiner", Harness: harness.Profile{Harness: f.h.name}}}})
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: tk.Ref, Project: "web", Role: "refiner"}); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	s := f.session(t)
	if s.spec.Workdir == f.dir(8) || exists(f.dir(8)) {
		t.Fatal("the refiner got the ticket's worktree")
	}
	if !exists(filepath.Join(s.spec.Workdir, "README.md")) {
		t.Fatalf("the refiner's directory %s lacks the main branch", s.spec.Workdir)
	}
	if branch := strings.TrimSpace(gitIn(t, s.spec.Workdir, "branch", "--show-current")); branch != "" {
		t.Fatalf("the refiner is on branch %q, want main detached", branch)
	}
	dir := s.spec.Workdir
	s.finish <- harness.Result{ExitCode: 1}
	f.waitEvent(t, "run.exited")
	for deadline := time.Now().Add(waitFor); exists(dir); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the refiner's checkout outlived its run")
		}
	}
}

func TestATranscriptRotatedIntoPartsIsReadWhole(t *testing.T) {
	t.Parallel()
	var events []harness.Event
	for i := range 6 {
		events = append(events, harness.Event{Kind: harness.EventMessage, Text: "message number " + strconv.Itoa(i)})
	}
	f := newRunner(t, runner.RunnerOptions{TranscriptMax: 120}, events...)
	f.readyTask(t, 9)
	f.start(t)
	s := f.session(t)
	s.finish <- harness.Result{ExitCode: 1}
	run, err := f.runner.Runs().Get(ctx, runOf(t, f.waitEvent(t, "run.exited")))
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := filepath.Glob(filepath.Join(filepath.Dir(run.Transcript), run.ID+"*.jsonl"))
	if len(parts) < 2 {
		t.Fatalf("transcript parts = %v, want it rotated", parts)
	}
	var got []string
	var pos runner.TranscriptPos
	for range 10 {
		lines, next, err := runner.ReadTranscript(run.Transcript, pos)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			var e harness.Event
			if err := json.Unmarshal(l, &e); err != nil {
				t.Fatal(err)
			}
			got = append(got, e.Text)
		}
		pos = next
	}
	if len(got) != 6 || got[0] != "message number 0" || got[5] != "message number 5" {
		t.Fatalf("read back %q, want the six messages in order", got)
	}
}

func TestTheInstructionComesFromTheMainBranchNotTheTicketBranch(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	// The repository's own implementer instruction on main.
	seed := filepath.Join(f.root, "seed")
	if err := os.MkdirAll(filepath.Join(seed, ".istari", "roles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".istari", "roles", "implementer.md"), []byte("# Our implementer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "our roles")
	gitIn(t, seed, "push", "-q", "origin", "main")
	tk, _ := f.readyTask(t, 12)
	// The ticket's branch rewrites it: that must not count.
	w, err := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")}).Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, ".istari", "roles", "implementer.md"), []byte("# Do whatever you like\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.Dir, "commit", "-q", "-am", "rewrite my rules")
	f.start(t)
	if s := f.session(t); s.spec.Instructions != "# Our implementer\n" {
		t.Fatalf("instructions = %q, want main's", s.spec.Instructions)
	}
}

func TestARunStoppedOnTimeoutLeavesACheckpointWithItsWork(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Timeout: 500 * time.Millisecond})
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	f.runner = runner.New(f.store, f.tasks, f.tickets, ws, runner.RunnerOptions{
		DataDir: f.data, Projects: []string{"web"}, Timeout: 500 * time.Millisecond,
		Slots:       []runner.Slot{{Profile: "implementer-1", Role: "implementer", Harness: harness.Profile{Harness: f.h.name}}},
		Checkpoints: runner.NewCheckpoints(ws, journal.New(nil, f.store, nil), nil),
	})
	_, task := f.readyTask(t, 13)
	f.start(t)
	s := f.session(t)
	if err := os.WriteFile(filepath.Join(s.spec.Workdir, "half-done.txt"), []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.waitEvent(t, "task.failed")
	remote := filepath.Join(f.root, "remotes", "acme", "web.git")
	branch := strings.TrimSpace(gitIn(t, s.spec.Workdir, "branch", "--show-current"))
	if files := gitIn(t, remote, "ls-tree", "--name-only", branch); !strings.Contains(files, "half-done.txt") {
		t.Fatalf("the remote branch has %q, want the work of the stopped run", files)
	}
	if events := f.events(t, "journal.checkpoint"); len(events) != 1 || events[0].Ticket != task.Ticket {
		t.Fatalf("checkpoint entries = %d, want one for the stopped run", len(events))
	}
}

func TestAHoldOnTheTicketStopsItsRunWithACheckpoint(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		hold, actor string
		issue       int
	}{
		{"paused", "human:alex", 14},
		{"waiting_dependency", "system", 16}, // the engine saw an open need mid-run
	} {
		t.Run(c.hold, func(t *testing.T) {
			t.Parallel()
			f := newRunner(t, runner.RunnerOptions{})
			ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
			f.runner = runner.New(f.store, f.tasks, f.tickets, ws, runner.RunnerOptions{
				DataDir: f.data, Projects: []string{"web"}, Lease: 600 * time.Millisecond,
				Slots:       []runner.Slot{{Profile: "implementer-1", Role: "implementer", Harness: harness.Profile{Harness: f.h.name}}},
				Checkpoints: runner.NewCheckpoints(ws, journal.New(nil, f.store, nil), nil),
			})
			tk, task := f.readyTask(t, c.issue)
			f.start(t)
			s := f.session(t)
			if err := os.WriteFile(filepath.Join(s.spec.Workdir, "held-work.txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tickets.SetHold(ctx, c.actor, tk.Ref, c.hold); err != nil {
				t.Fatal(err)
			}
			f.waitTask(t, task.ID, core.TaskReady) // released, and not taken again while held
			if events := f.events(t, "journal.checkpoint"); len(events) != 1 || events[0].Ticket != tk.Ref {
				t.Fatalf("checkpoints = %d, want one for the stopped run", len(events))
			}
			time.Sleep(300 * time.Millisecond)
			got, err := f.tasks.Get(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != core.TaskReady {
				t.Fatalf("the held ticket's task is %s, want ready and left alone", got.State)
			}
		})
	}
}

func TestCancelingTheTicketStopsItsRunWithACheckpointAndTheTaskStaysCanceled(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	f.runner = runner.New(f.store, f.tasks, f.tickets, ws, runner.RunnerOptions{
		DataDir: f.data, Projects: []string{"web"}, Lease: 600 * time.Millisecond,
		Slots:       []runner.Slot{{Profile: "implementer-1", Role: "implementer", Harness: harness.Profile{Harness: f.h.name}}},
		Checkpoints: runner.NewCheckpoints(ws, journal.New(nil, f.store, nil), nil),
	})
	tk, task := f.readyTask(t, 15)
	f.start(t)
	s := f.session(t)
	if err := os.WriteFile(filepath.Join(s.spec.Workdir, "canceled-work.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tickets.Fire(ctx, "human:alex", tk.Ref, "cancel"); err != nil {
		t.Fatal(err)
	}
	f.waitEvent(t, runner.EventRunExited)
	if events := f.events(t, "journal.checkpoint"); len(events) != 1 {
		t.Fatalf("checkpoints = %d, want one on canceling", len(events))
	}
	got, err := f.tasks.Get(ctx, task.ID)
	if err != nil || got.State != core.TaskCanceled || got.Attempts != 0 {
		t.Fatalf("the task is %s with %d attempts (%v), want canceled with none spent", got.State, got.Attempts, err)
	}
	if failed := f.events(t, "task.failed"); len(failed) != 0 {
		t.Fatalf("the canceled ticket's run failed its task: %v", failed)
	}
}

// pausedWithSlowChecks starts a run of ticket #n whose main branch's
// typecheck is check, writes a file in its worktree and pauses the ticket.
// hooks are the remote's hooks by name: pre-receive runs before the remote
// takes the push, post-receive after it took it and before the push returns.
// It returns the session and the task.
func pausedWithSlowChecks(t *testing.T, n int, check string, hooks map[string]string, opts runner.RunnerOptions) (*runFixture, fakeSession, core.Task) {
	t.Helper()
	f := newRunner(t, runner.RunnerOptions{Lease: opts.Lease})
	seedMain(t, f, ".istari/pipeline.toml", "[checks]\ntypecheck = \""+check+"\"\ntest = \"true\"\nverify_red = false\n")
	for name, script := range hooks {
		hook := filepath.Join(f.root, "remotes", "acme", "web.git", "hooks", name)
		if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	opts.DataDir, opts.Projects = f.data, []string{"web"}
	opts.Slots = []runner.Slot{{Profile: "implementer-1", Role: "implementer", Harness: harness.Profile{Harness: f.h.name}}}
	opts.Checkpoints = runner.NewCheckpoints(ws, journal.New(nil, f.store, nil), nil)
	f.runner = runner.New(f.store, f.tasks, f.tickets, ws, opts)
	tk, task := f.readyTask(t, n)
	f.start(t)
	s := f.session(t)
	if err := os.WriteFile(filepath.Join(s.spec.Workdir, "paused-work.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tickets.SetHold(ctx, "human:alex", tk.Ref, "paused"); err != nil {
		t.Fatal(err)
	}
	return f, s, task
}

func TestAStoppingRunKeepsItsLeaseWhileItsCheckpointOutlastsIt(t *testing.T) {
	t.Parallel()
	// The check holds the checkpoint until the run renewed the lease past
	// one lease length after the check started, so a run that stopped
	// renewing would lose it.
	gates := t.TempDir()
	checking, release := filepath.Join(gates, "checking"), filepath.Join(gates, "release")
	check := "touch '" + checking + "'; while [ ! -e '" + release + "' ]; do sleep 0.05; done"
	f, s, task := pausedWithSlowChecks(t, 17, check, nil, runner.RunnerOptions{Lease: stopLease})
	// The scheduler expires leases on its ticks; here the test does.
	// The state is read before the events: a task that leaves claimed
	// between the two reads is then seen with its task.released event.
	var checked time.Time // when the test saw the check start
	released := false     // whether the test let the check end
	for deadline := time.Now().Add(2 * waitFor); ; {
		if time.Now().After(deadline) {
			t.Fatalf("the paused run did not release its task (check started: %v, let end: %v)", !checked.IsZero(), released)
		}
		if expired, err := f.tasks.ExpireLeases(ctx); err != nil || len(expired) > 0 {
			t.Fatalf("leases expired while the run stopped: %v, %v", expired, err)
		}
		got, err := f.tasks.Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.events(t, "task.released")) > 0 {
			break
		}
		if got.State != core.TaskClaimed {
			t.Fatalf("the stopping run's task is %s, want claimed until the run ended", got.State)
		}
		if checked.IsZero() && exists(checking) {
			checked = time.Now()
		}
		if !released && !checked.IsZero() && renewedAfter(ctx, f, checked.Add(stopLease)) {
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			released = true
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !released {
		t.Fatal("the run released its task before its checkpoint outlasted the lease")
	}
	f.waitTask(t, task.ID, core.TaskReady)
	if events := f.events(t, "journal.checkpoint"); len(events) != 1 {
		t.Fatalf("checkpoints = %d, want one on pausing", len(events))
	}
	remote := filepath.Join(f.root, "remotes", "acme", "web.git")
	branch := strings.TrimSpace(gitIn(t, s.spec.Workdir, "branch", "--show-current"))
	if files := gitIn(t, remote, "ls-tree", "--name-only", branch); !strings.Contains(files, "paused-work.txt") {
		t.Fatalf("the remote branch has %q, want the work of the paused run", files)
	}
}

// stopLease is the lease of the tests of a stopping run; the run sees the
// hold on a renewal, every third of it. A lease this long does not lapse
// when a loaded machine, as CI's race job, stalls the renewals for a moment.
const stopLease = 5 * time.Second

// stopLimit is the stop checkpoint limit of the tests that reach it. It is
// long enough for the commit and the checks before the hook or check that
// the test blocks, however loaded the machine: the limit is reached only
// while that blocks.
const stopLimit = 30 * time.Second

// blocks is a shell script that touches started and then blocks past
// stopLimit; the checkpoint kills it at the limit.
func blocks(started string) string {
	return "touch '" + started + "'; sleep 90"
}

func TestAStopCheckpointPastItsLimitIsAbandonedAndTheWorkStaysInTheWorktree(t *testing.T) {
	t.Parallel()
	checking := filepath.Join(t.TempDir(), "checking")
	f, s, task := pausedWithSlowChecks(t, 18, blocks(checking), nil, runner.RunnerOptions{Lease: stopLease, StopCheckpoint: stopLimit})
	abandonedAtItsLimit(t, f, s, task, stopLimit)
	if !exists(checking) {
		t.Fatal("the check never started, want the limit reached while checking")
	}
}

func TestAStopCheckpointWhosePushHangsPastItsLimitIsAbandoned(t *testing.T) {
	t.Parallel()
	// The remote holds the push past the limit and then refuses it.
	pushing := filepath.Join(t.TempDir(), "pushing")
	hooks := map[string]string{"pre-receive": blocks(pushing) + "; exit 1"}
	f, s, task := pausedWithSlowChecks(t, 20, "true", hooks, runner.RunnerOptions{Lease: stopLease, StopCheckpoint: stopLimit})
	abandonedAtItsLimit(t, f, s, task, stopLimit)
	if !exists(pushing) {
		t.Fatal("the push never reached the remote, want the limit reached while pushing")
	}
}

// abandonedAtItsLimit checks that a paused run whose stop checkpoint
// outlasts limit ends, its task released, with the work uncommitted in the
// worktree, nothing pushed, no checkpoint and a journal note naming the
// limit.
func abandonedAtItsLimit(t *testing.T, f *runFixture, s fakeSession, task core.Task, limit time.Duration) {
	t.Helper()
	head := strings.TrimSpace(gitIn(t, s.spec.Workdir, "rev-parse", "HEAD"))
	f.waitTaskWithin(t, task.ID, core.TaskReady, limit+waitFor)
	f.waitEvent(t, "run.exited")
	if got := strings.TrimSpace(gitIn(t, s.spec.Workdir, "rev-parse", "HEAD")); got != head {
		t.Errorf("HEAD = %s, want %s as before the checkpoint", got, head)
	}
	if status := gitIn(t, s.spec.Workdir, "status", "--porcelain"); !strings.Contains(status, "paused-work.txt") {
		t.Errorf("git status = %q, want the work uncommitted in the worktree", status)
	}
	remote := filepath.Join(f.root, "remotes", "acme", "web.git")
	branch := strings.TrimSpace(gitIn(t, s.spec.Workdir, "branch", "--show-current"))
	// for-each-ref lists nothing, and does not fail, when nothing ever
	// pushed the branch.
	if pushed := gitIn(t, remote, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch); pushed != "" {
		if files := gitIn(t, remote, "ls-tree", "--name-only", "refs/heads/"+branch); strings.Contains(files, "paused-work.txt") {
			t.Errorf("the remote branch has %q, want nothing pushed", files)
		}
	}
	if events := f.events(t, "journal.checkpoint"); len(events) != 0 {
		t.Errorf("checkpoints = %d, want none for an abandoned one", len(events))
	}
	ref, err := tickets.ParseRef(task.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := journal.New(nil, f.store, nil).Stored(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	for _, e := range entries {
		noted = noted || e.Kind == journal.Note && strings.Contains(e.Text, "limit of "+limit.String()) && strings.Contains(e.Text, "stop_checkpoint_timeout") && strings.Contains(e.Text, "uncommitted")
	}
	if !noted {
		t.Fatalf("journal = %+v, want a note naming the limit and the uncommitted work", entries)
	}
}

func TestAStopCheckpointWhosePushOutlastsItsLimitIsKept(t *testing.T) {
	t.Parallel()
	// The remote takes the push at once and answers after the limit: what
	// it took is the checkpoint, not abandoned work.
	taken := filepath.Join(t.TempDir(), "taken")
	f, s, task := pausedWithSlowChecks(t, 19, "true", map[string]string{"post-receive": blocks(taken)}, runner.RunnerOptions{Lease: stopLease, StopCheckpoint: stopLimit})
	f.waitTaskWithin(t, task.ID, core.TaskReady, stopLimit+waitFor)
	f.waitEvent(t, "run.exited")
	if !exists(taken) {
		t.Fatal("the remote never took the push, want the limit reached after it took it")
	}
	if events := f.events(t, "journal.checkpoint"); len(events) != 1 {
		t.Fatalf("checkpoints = %d, want the pushed one", len(events))
	}
	if status := gitIn(t, s.spec.Workdir, "status", "--porcelain"); status != "" {
		t.Errorf("git status = %q, want the work committed", status)
	}
	remote := filepath.Join(f.root, "remotes", "acme", "web.git")
	branch := strings.TrimSpace(gitIn(t, s.spec.Workdir, "branch", "--show-current"))
	if head, pushed := gitIn(t, s.spec.Workdir, "rev-parse", "HEAD"), gitIn(t, remote, "rev-parse", "refs/heads/"+branch); head != pushed {
		t.Errorf("remote branch = %s, want the worktree's HEAD %s", pushed, head)
	}
}

func TestARejectedResultFailsTheRunEvenAfterComplete(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	_, task := f.readyTask(t, 15)
	f.start(t)
	s := f.session(t)
	if _, err := f.runner.Runs().Reject(ctx, s.spec.Env["ISTARI_RUN"], "refine result: size \"huge\" is not a size"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Runs().Complete(ctx, s.spec.Env["ISTARI_RUN"], "done"); err != nil {
		t.Fatal(err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	if failed := f.waitEvent(t, "task.failed"); !strings.Contains(string(failed.Payload), "huge") {
		t.Fatalf("task.failed = %s, want the rejection", failed.Payload)
	}
	if failed := f.waitEvent(t, "task.failed"); failed.Task != task.ID && failed.Ticket != task.Ticket {
		t.Fatalf("the failure is of another task: %+v", failed)
	}
}

func TestAResultAcceptedAfterARefusalInTheSameRunCompletesTheTask(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	_, task := f.readyTask(t, 16)
	f.start(t)
	s := f.session(t)
	id := s.spec.Env["ISTARI_RUN"]
	if _, err := f.runner.Runs().Reject(ctx, id, "review: yaml: line 3: did not find expected key"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Runs().Accept(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Runs().Complete(ctx, id, "reviewed"); err != nil {
		t.Fatal(err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	f.waitTask(t, task.ID, core.TaskCompleted)
	if run, _ := f.runner.Runs().Get(ctx, id); run.Reason != "completed: reviewed" {
		t.Fatalf("the run ended %q, want it completed: its refused result was corrected", run.Reason)
	}
	if rejected := f.events(t, "run.rejected"); len(rejected) != 1 || !strings.Contains(string(rejected[0].Payload), "yaml") {
		t.Fatalf("run.rejected = %+v, want the refusal kept", rejected)
	}
}

func TestAYieldedRunReleasesItsTaskWithoutSpendingAnAttempt(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	_, task := f.readyTask(t, 15)
	f.start(t)
	s := f.session(t)
	if _, err := f.runner.Runs().Yield(ctx, s.spec.Env["ISTARI_RUN"], "gh:o/r#15 is in_progress; a review is submitted in in_review"); err != nil {
		t.Fatal(err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	// Released to ready; the slot may claim it again at once.
	if released := f.waitEvent(t, "task.released"); released.Task != task.ID {
		t.Fatalf("task.released is of %q, want %q", released.Task, task.ID)
	}
	if got, _ := f.tasks.Get(ctx, task.ID); got.Attempts != 0 {
		t.Fatalf("attempts = %d after a yielded run, want 0", got.Attempts)
	}
}

func TestARejectedRunSpendsAnAttemptEvenIfItAlsoYielded(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	_, task := f.readyTask(t, 15)
	f.start(t)
	s := f.session(t)
	id := s.spec.Env["ISTARI_RUN"]
	if _, err := f.runner.Runs().Yield(ctx, id, "gh:o/r#15 is in_progress"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Runs().Reject(ctx, id, "review result: yaml: line 1"); err != nil {
		t.Fatal(err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	f.waitEvent(t, "task.failed")
	if got, _ := f.tasks.Get(ctx, task.ID); got.Attempts != 1 {
		t.Fatalf("attempts = %d after a rejected run, want 1", got.Attempts)
	}
}

func TestARunStartsWithAWholeLeaseHoweverLongItsPreparationTook(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Lease: time.Hour})
	_, task := f.readyTask(t, 11)
	claimedAt := time.Now()
	f.start(t)
	f.session(t)
	got := f.waitTask(t, task.ID, core.TaskClaimed)
	var started time.Time
	for _, e := range f.events(t, "task.renewed") {
		started = e.At
	}
	if started.IsZero() || got.LeaseUntil.Before(started.Add(time.Hour-time.Second)) || !started.After(claimedAt) {
		t.Fatalf("the run started on the lease of its claim: until %s, renewed at %s", got.LeaseUntil, started)
	}
}

func TestARunWhosePreparationOutlastsTheLeaseStillStarts(t *testing.T) {
	t.Parallel()
	// The lease is long enough for its renewals (every third of it) to
	// survive a stall of a loaded CI box; the preparation outlasts it by
	// design, not by load: assembling the brief lasts until a renewal is
	// dated after the lease of the claim ran out.
	const lease = 1500 * time.Millisecond
	var f *runFixture
	f = newRunner(t, runner.RunnerOptions{Lease: lease, SelfReview: func(ctx context.Context, _ string) bool {
		for deadline := time.Now().Add(waitFor); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if renewedPastTheClaim(ctx, f, lease) {
				break
			}
		}
		return true
	}})
	_, task := f.readyTask(t, 12)
	f.start(t)
	s := f.session(t)
	defer s.Stop(0) //nolint:errcheck // the fake session's Stop always returns nil
	if !renewedPastTheClaim(ctx, f, lease) {
		t.Fatal("the preparation did not outlast the lease of the claim")
	}
	if s.spec.Env["ISTARI_TASK"] != task.ID || len(f.events(t, "task.expired")) != 0 {
		t.Fatalf("the run of %s lost its lease while it was prepared: %d expirations", task.ID, len(f.events(t, "task.expired")))
	}
}

// renewedPastTheClaim tells whether a task.renewed event is dated after the
// lease of the first task.claimed event ran out.
func renewedPastTheClaim(ctx context.Context, f *runFixture, lease time.Duration) bool {
	claims, err := f.store.Read(ctx, "", store.Filter{TypePrefix: "task.claimed"}, 0)
	if err != nil || len(claims) == 0 {
		return false
	}
	return renewedAfter(ctx, f, claims[0].At.Add(lease))
}

// waitRenewedPastTheClaim waits until a task.renewed event is dated after
// the lease of the first task.claimed event ran out.
func (f *runFixture) waitRenewedPastTheClaim(t *testing.T, lease time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(waitFor); !renewedPastTheClaim(ctx, f, lease); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the lease was not renewed past the lease of the claim")
		}
	}
}

// renewedAfter tells whether a task.renewed event is dated after at.
func renewedAfter(ctx context.Context, f *runFixture, at time.Time) bool {
	renewals, err := f.store.Read(ctx, "", store.Filter{TypePrefix: "task.renewed"}, 0)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(renewals, func(e store.Event) bool { return e.At.After(at) })
}

// seedMain commits a file to the main branch of the fixture's repository.
func seedMain(t *testing.T, f *runFixture, path, content string) {
	t.Helper()
	seed := filepath.Join(f.root, "seed")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(seed, path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, path), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "seed "+path)
	gitIn(t, seed, "push", "-q", "origin", "main")
}

func TestTheImplementerMayRunTheCheckCommandsOfTheMainBranch(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	seedMain(t, f, ".istari/pipeline.toml", "[checks]\ntypecheck = \"go vet ./...\"\ntest = \"go test ./... -short\"\nverify_red = true\n")
	tk, _ := f.readyTask(t, 15)
	// The ticket's branch adds a check of its own: that must not widen what
	// the agent may run.
	w, err := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")}).Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, ".istari", "pipeline.toml"), []byte("[checks]\ntest = \"go test ./...\"\nlint = \"curl https://example.com/x.sh\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.Dir, "commit", "-q", "-am", "my own checks")
	f.start(t)
	got := f.session(t).spec.AllowedTools
	for _, want := range []string{"Bash(go test *)", "Bash(go vet *)"} {
		if !slices.Contains(got, want) {
			t.Errorf("allowed tools = %q, want %q among them", got, want)
		}
	}
	if slices.Contains(got, "Bash(curl *)") {
		t.Errorf("allowed tools = %q, want no command from the ticket's branch", got)
	}
}

func TestTheImplementerMayRunTheProjectsOwnNamedChecks(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	seedMain(t, f, ".istari/pipeline.toml", "[checks]\ntest = \"go test -short {dirs}\"\nformat = \"gofmt -l .\"\nlint = { run = \"cargo clippy -- -D warnings\", at = [\"pr\"] }\nunit = \"npx vitest run {files}\"\n")
	f.readyTask(t, 16)
	f.start(t)
	got := f.session(t).spec.AllowedTools
	for _, want := range []string{"Bash(go test *)", "Bash(gofmt *)", "Bash(cargo clippy *)", "Bash(npx vitest run *)"} {
		if !slices.Contains(got, want) {
			t.Errorf("allowed tools = %q, want %q among them", got, want)
		}
	}
}

func TestEveryRolesBriefListsEveryCommandItsHarnessAllows(t *testing.T) {
	t.Parallel()
	for i, role := range []string{"refiner", "planner", "implementer", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			f := newRunner(t, runner.RunnerOptions{})
			seedMain(t, f, ".istari/pipeline.toml", "[checks]\ntest = \"go test ./... -short\"\n")
			profile := claudecode.DefaultProfiles()[role]
			profile.Harness = f.h.name
			f.runner = runner.New(f.store, f.tasks, f.tickets, runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")}),
				runner.RunnerOptions{DataDir: f.data, Projects: []string{"web"}, Slots: []runner.Slot{{Profile: role + "-1", Role: role, Harness: profile}}})
			tk := f.ticket(t, 20+i, "Brief me")
			if role != "refiner" {
				f.fire(t, tk.Ref, "accept", "start")
			}
			if _, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: tk.Ref, Project: "web", Role: role}); err != nil {
				t.Fatal(err)
			}
			f.start(t)
			s := f.session(t)
			commands := 0
			for _, tool := range s.spec.AllowedTools {
				cmd, ok := strings.CutPrefix(tool, "Bash(")
				if !ok {
					continue
				}
				commands++
				if want := "- `" + strings.TrimSuffix(cmd, ")") + "`\n"; !strings.Contains(s.spec.Brief, want) {
					t.Errorf("the brief does not list %s, which the harness allows", tool)
				}
			}
			if commands == 0 {
				t.Fatalf("allowed tools = %q, want shell commands to check", s.spec.AllowedTools)
			}
			if role == "implementer" && !strings.Contains(s.spec.Brief, "- `go test *`\n") {
				t.Error("the implementer's brief does not list the check command go test")
			}
		})
	}
}

func TestAPlannerMayWriteItsResultFileInsideItsRunDirectoryAndNoOtherFile(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	f.runner = runner.New(f.store, f.tasks, f.tickets, ws, runner.RunnerOptions{
		DataDir: f.data, Projects: []string{"web"},
		Slots: []runner.Slot{{Profile: "planner-1", Role: "planner", Harness: harness.Profile{Harness: f.h.name, AllowedTools: []string{"Read", "Bash(istari *)"}}}},
	})
	tk := f.ticket(t, 17, "Plan me")
	f.fire(t, tk.Ref, "accept", "start")
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: tk.Ref, Project: "web", Role: "planner"}); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	spec := f.session(t).spec
	result := spec.Env["ISTARI_RESULT"]
	if want := filepath.Join(f.data, "runs", spec.Env["ISTARI_RUN"], "result.yaml"); result != want {
		t.Fatalf("ISTARI_RESULT = %q, want %q in the run's directory", result, want)
	}
	want := []string{"Read", "Bash(istari *)", "Write(/" + result + ")", "Edit(/" + result + ")"}
	if !slices.Equal(spec.AllowedTools, want) {
		t.Fatalf("a planner's allowed tools = %q, want %q: its profile and the result file", spec.AllowedTools, want)
	}
}

func TestTheImplementerGetsNoResultFile(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	f.readyTask(t, 18)
	f.start(t)
	spec := f.session(t).spec
	if v, ok := spec.Env["ISTARI_RESULT"]; ok {
		t.Fatalf("the implementer's ISTARI_RESULT = %q, want none", v)
	}
	for _, tool := range spec.AllowedTools {
		if strings.HasPrefix(tool, "Write(") || strings.HasPrefix(tool, "Edit(") {
			t.Fatalf("the implementer's allowed tools = %q, want no rule for a single file", spec.AllowedTools)
		}
	}
}

func TestOtherRolesDoNotGetTheCheckCommands(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	seedMain(t, f, ".istari/pipeline.toml", "[checks]\ntest = \"go test ./...\"\n")
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	f.runner = runner.New(f.store, f.tasks, f.tickets, ws, runner.RunnerOptions{
		DataDir: f.data, Projects: []string{"web"},
		Slots: []runner.Slot{{Profile: "planner-1", Role: "planner", Harness: harness.Profile{Harness: f.h.name}}},
	})
	tk := f.ticket(t, 16, "Plan me")
	f.fire(t, tk.Ref, "accept", "start")
	if _, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: tk.Ref, Project: "web", Role: "planner"}); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	if got := f.session(t).spec.AllowedTools; slices.Contains(got, "Bash(go test *)") {
		t.Fatalf("a planner's allowed tools = %q, want no check command", got)
	}
}
