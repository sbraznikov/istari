package runner_test

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
)

// errMissing is how a harness binary that is being replaced fails to start.
var errMissing = errors.New("start claude: fork/exec /Users/pc/.local/bin/claude: no such file or directory")

// The waits a slot makes after its harness did not start, at their defaults;
// the tests run on the clock of a synctest bubble, so they cost no real time.
const (
	retry = runner.DefaultStartRetry
	pause = runner.DefaultStartPause
)

// daemonLog captures what the runner writes to the daemon log.
type daemonLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *daemonLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// line returns the first line containing want, or "".
func (l *daemonLog) line(want string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for line := range strings.SplitSeq(l.buf.String(), "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}

func captureLog(t *testing.T) *daemonLog {
	t.Helper()
	l := &daemonLog{}
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return l
}

// after lets the bubble's clock run for d, then waits until every goroutine
// of the runner is blocked again.
func after(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func TestAHarnessThatDoesNotStartReleasesTheTaskWithoutSpendingAnAttempt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newRunner(t, runner.RunnerOptions{})
		f.h.failStart(errMissing)
		_, task := f.readyTask(t, 1)
		f.start(t)
		after(retry - time.Second)
		if released := f.events(t, "task.released"); len(released) != 0 {
			t.Fatalf("task.released before the wait of %s, want it after", retry)
		}
		after(2 * time.Second)
		if released := f.events(t, "task.released"); len(released) == 0 || released[0].Task != task.ID {
			t.Fatalf("task.released = %+v, want one of %q", released, task.ID)
		}
		if got, _ := f.tasks.Get(ctx, task.ID); got.Attempts != 0 {
			t.Fatalf("attempts = %d after a harness that did not start, want 0", got.Attempts)
		}
		if failed := f.events(t, "task.failed"); len(failed) != 0 {
			t.Fatalf("task.failed = %s, want none", failed[0].Payload)
		}
	})
}

func TestThreeStartFailuresInARowPauseTheSlotAndTheLogSaysWhy(t *testing.T) {
	// Not parallel: sets the output of the process's standard logger.
	logged := captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		f := newRunner(t, runner.RunnerOptions{})
		f.h.failStart(errMissing)
		_, task := f.readyTask(t, 1)
		f.start(t)
		after(3*retry + time.Second)
		want := "runner implementer-1: paused for 1m: the harness did not start: " + errMissing.Error()
		if line := logged.line("paused for"); !strings.Contains(line, want) {
			t.Fatalf("log line = %q, want it to contain %q", line, want)
		}
		after(pause - 2*time.Second)
		if claims := f.events(t, "task.claimed"); len(claims) != 3 {
			t.Fatalf("%d claims during the pause, want 3", len(claims))
		}
		after(2 * time.Second)
		if claims := f.events(t, "task.claimed"); len(claims) != 4 {
			t.Fatalf("%d claims after the pause, want 4", len(claims))
		}
		if got, _ := f.tasks.Get(ctx, task.ID); got.Attempts != 0 {
			t.Fatalf("attempts = %d, want 0", got.Attempts)
		}
	})
}

func TestPausedTellsWhichSlotIsPausedWhyAndUntilWhen(t *testing.T) {
	// Not parallel: sets the output of the process's standard logger.
	captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		f := newRunner(t, runner.RunnerOptions{})
		if paused := f.runner.Paused(); len(paused) != 0 {
			t.Fatalf("paused = %+v before any failure, want none", paused)
		}
		f.h.failStart(errMissing)
		f.readyTask(t, 1)
		began := time.Now()
		f.start(t)
		after(3*retry + time.Second)
		paused := f.runner.Paused()
		if len(paused) != 1 || paused[0].Slot != "implementer-1" || !strings.Contains(paused[0].Error, errMissing.Error()) {
			t.Fatalf("paused = %+v, want implementer-1 with %q", paused, errMissing)
		}
		// Three claims, each held for the retry wait, then the pause.
		if until, want := paused[0].Until, began.Add(3*retry+pause); !until.Equal(want) {
			t.Fatalf("until = %s, want %s", until, want)
		}
		f.h.failStart(nil)
		after(pause)
		f.session(t)
		if paused := f.runner.Paused(); len(paused) != 0 {
			t.Fatalf("paused = %+v after the slot started a run, want none", paused)
		}
	})
}

func TestOnceTheHarnessStartsAgainTheNextClaimAfterThePauseStartsARun(t *testing.T) {
	// Not parallel: sets the output of the process's standard logger.
	captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		f := newRunner(t, runner.RunnerOptions{})
		f.h.failStart(errMissing)
		_, task := f.readyTask(t, 1)
		f.start(t)
		after(3*retry + time.Second)
		f.h.failStart(nil)
		after(pause)
		if s := f.session(t); s.spec.Env["ISTARI_TASK"] != task.ID {
			t.Fatalf("the run is of task %q, want %q", s.spec.Env["ISTARI_TASK"], task.ID)
		}
	})
}

// count returns how many lines contain want.
func (l *daemonLog) count(want string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), want)
}

func TestASlotStartsNoRunOnATicketWithARunAndTheLogSaysWhyOnce(t *testing.T) {
	// Not parallel: sets the output of the process's standard logger.
	logged := captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		f := newRunner(t, runner.RunnerOptions{})
		tk, running := f.readyTask(t, 1)
		// Another runner's slot has a run of the ticket's first task.
		elsewhere, err := f.tasks.Claim(ctx, "agent:implementer-2", running.ID)
		if err != nil {
			t.Fatal(err)
		}
		waiting, err := f.tasks.Create(ctx, "system", core.NewTask{Ticket: tk.Ref, Project: "web", Role: "implementer"})
		if err != nil {
			t.Fatal(err)
		}
		f.start(t)
		after(5 * runner.DefaultPoll)
		if claims := f.events(t, "task.claimed"); len(claims) != 1 {
			t.Fatalf("%d claims while the ticket has a run, want only the first", len(claims))
		}
		want := "runner implementer-1: ticket_busy: ticket " + tk.Ref + " has a run of task " + running.ID
		if n := logged.count(want); n != 1 {
			t.Fatalf("the log says %d times %q, want once:\n%s", n, want, logged.buf.String())
		}
		if err := f.tasks.Complete(ctx, elsewhere); err != nil {
			t.Fatal(err)
		}
		after(runner.DefaultPoll)
		if s := f.session(t); s.spec.Env["ISTARI_TASK"] != waiting.ID {
			t.Fatalf("the run is of task %q, want the waiting %q", s.spec.Env["ISTARI_TASK"], waiting.ID)
		}
	})
}
