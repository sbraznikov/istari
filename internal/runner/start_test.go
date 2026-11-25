package runner_test

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/runner"
)

// errMissing is how a harness binary that is being replaced fails to start.
var errMissing = errors.New("start claude: fork/exec /Users/pc/.local/bin/claude: no such file or directory")

// daemonLog captures what the runner writes to the daemon log.
type daemonLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *daemonLog) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.Write(p) }
func (l *daemonLog) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

func captureLog(t *testing.T) *daemonLog {
	t.Helper()
	l := &daemonLog{}
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return l
}

// waitLog waits until the daemon log has a line containing want and returns it.
func waitLog(t *testing.T, l *daemonLog, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, line := range strings.Split(l.String(), "\n") {
			if strings.Contains(line, want) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line with %q in:\n%s", want, l.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAHarnessThatDoesNotStartReleasesTheTaskWithoutSpendingAnAttempt(t *testing.T) {
	f := newRunner(t, runner.RunnerOptions{StartRetry: 10 * time.Millisecond})
	f.h.failStart(errMissing)
	_, task := f.readyTask(t, 1)
	f.start(t)
	if released := f.waitEvent(t, "task.released"); released.Task != task.ID {
		t.Fatalf("task.released is of %q, want %q", released.Task, task.ID)
	}
	if got, _ := f.tasks.Get(ctx, task.ID); got.Attempts != 0 {
		t.Fatalf("attempts = %d after a harness that did not start, want 0", got.Attempts)
	}
	if failed := f.events(t, "task.failed"); len(failed) != 0 {
		t.Fatalf("task.failed = %s, want none", failed[0].Payload)
	}
}

func TestThreeStartFailuresInARowPauseTheSlotAndTheLogSaysWhy(t *testing.T) {
	logged := captureLog(t)
	f := newRunner(t, runner.RunnerOptions{StartRetry: time.Millisecond, StartPause: 400 * time.Millisecond})
	f.h.failStart(errMissing)
	_, task := f.readyTask(t, 1)
	f.start(t)
	line := waitLog(t, logged, "paused for")
	if want := "runner implementer-1: paused for 400ms: the harness did not start: " + errMissing.Error(); !strings.Contains(line, want) {
		t.Fatalf("log line = %q, want it to contain %q", line, want)
	}
	time.Sleep(200 * time.Millisecond) // within the pause
	if claims := f.events(t, "task.claimed"); len(claims) != 3 {
		t.Fatalf("%d claims while the slot is paused, want the 3 before the pause", len(claims))
	}
	if got, _ := f.tasks.Get(ctx, task.ID); got.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0", got.Attempts)
	}
}

func TestPausedTellsWhichSlotIsPausedWhyAndUntilWhen(t *testing.T) {
	logged := captureLog(t)
	f := newRunner(t, runner.RunnerOptions{StartRetry: time.Millisecond, StartPause: 300 * time.Millisecond})
	if paused := f.runner.Paused(); len(paused) != 0 {
		t.Fatalf("paused = %+v before any failure, want none", paused)
	}
	f.h.failStart(errMissing)
	f.readyTask(t, 1)
	before := time.Now()
	f.start(t)
	waitLog(t, logged, "paused for")
	paused := f.runner.Paused()
	if len(paused) != 1 || paused[0].Slot != "implementer-1" || !strings.Contains(paused[0].Error, errMissing.Error()) {
		t.Fatalf("paused = %+v, want implementer-1 with %q", paused, errMissing)
	}
	if until := paused[0].Until; until.Before(before.Add(300*time.Millisecond)) || until.After(time.Now().Add(300*time.Millisecond)) {
		t.Fatalf("until = %s, want 300ms after the pause began", until)
	}
	f.h.failStart(nil)
	f.session(t)
	if paused := f.runner.Paused(); len(paused) != 0 {
		t.Fatalf("paused = %+v after the slot started a run, want none", paused)
	}
}

func TestOnceTheHarnessStartsAgainTheNextClaimAfterThePauseStartsARun(t *testing.T) {
	logged := captureLog(t)
	f := newRunner(t, runner.RunnerOptions{StartRetry: time.Millisecond, StartPause: 200 * time.Millisecond})
	f.h.failStart(errMissing)
	_, task := f.readyTask(t, 1)
	f.start(t)
	waitLog(t, logged, "paused for")
	f.h.failStart(nil)
	if s := f.session(t); s.spec.Env["ISTARI_TASK"] != task.ID {
		t.Fatalf("the run is of task %q, want %q", s.spec.Env["ISTARI_TASK"], task.ID)
	}
}
