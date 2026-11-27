package runner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
)

// TestMain lets a test run a runner in a process of its own, to kill it.
func TestMain(m *testing.M) {
	if db := os.Getenv("RUNNER_CHILD_DB"); db != "" {
		childRunner(db, os.Getenv("RUNNER_CHILD_DATA"), os.Getenv("RUNNER_CHILD_REMOTES"))
		return
	}
	os.Exit(m.Run())
}

// childRunner runs one slot whose harness never ends, until it is killed.
func childRunner(db, data, remotes string) {
	s, err := store.Open(db)
	if err != nil {
		panic(err)
	}
	h := newFakeHarness("fake-child")
	tasks := core.NewTasks(s, core.TaskOptions{Lease: 600 * time.Millisecond})
	ws := runner.NewWorkspaces(s, runner.WorkspaceOptions{DataDir: data, Server: "file://" + remotes})
	r := runner.New(s, tasks, core.NewTickets(s, core.TicketOptions{}), ws, runner.RunnerOptions{
		DataDir: data, Projects: []string{"web"}, Lease: 600 * time.Millisecond, Poll: 50 * time.Millisecond,
		Slots: []runner.Slot{{Profile: "implementer-1", Role: "implementer", Harness: harness.Profile{Harness: h.name}}},
	})
	go func() {
		for range h.started { // the session hangs: the run is alive until the kill
		}
	}()
	r.Run(ctx)
}

func TestAfterTheRunnerIsKilledTheTaskExpiresAndAnotherSlotTakesIt(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{Lease: 600 * time.Millisecond, Poll: 50 * time.Millisecond})
	_, task := f.readyTask(t, 6)
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "RUNNER_CHILD_DB="+filepath.Join(f.storeDir, "istari.db"), "RUNNER_CHILD_DATA="+f.data, "RUNNER_CHILD_REMOTES="+filepath.Join(f.root, "remotes"))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Process.Kill() //nolint:errcheck // the test has usually killed it already
		child.Wait()         //nolint:errcheck // it reports the kill
	})
	if got := f.waitTask(t, task.ID, core.TaskClaimed); got.Owner != "agent:implementer-1" {
		t.Fatalf("owner = %s, want the child's implementer-1", got.Owner)
	}
	time.Sleep(time.Second) // the child renews: still its own
	if got, _ := f.tasks.Get(ctx, task.ID); got.State != core.TaskClaimed || got.Owner != "agent:implementer-1" {
		t.Fatalf("while the child lives the task is %s by %s", got.State, got.Owner)
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	child.Wait() //nolint:errcheck // it reports the kill

	time.Sleep(700 * time.Millisecond) // past the last lease
	if expired, err := f.tasks.ExpireLeases(ctx); err != nil || len(expired) != 1 {
		t.Fatalf("ExpireLeases = %v, %v; want the killed runner's task", expired, err)
	}
	f.runner = runner.New(f.store, f.tasks, f.tickets, runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")}),
		// The lease of f.tasks, so the runner renews it while the run is
		// prepared, however long that takes under parallel tests.
		runner.RunnerOptions{DataDir: f.data, Projects: []string{"web"}, Lease: 600 * time.Millisecond, Poll: 50 * time.Millisecond,
			Slots: []runner.Slot{{Profile: "implementer-2", Role: "implementer", Harness: harness.Profile{Harness: f.h.name}}}})
	f.start(t)
	f.session(t)
	if got := f.waitTask(t, task.ID, core.TaskClaimed); got.Owner != "agent:implementer-2" {
		t.Fatalf("after the expiry the task is held by %s, want implementer-2", got.Owner)
	}
}
