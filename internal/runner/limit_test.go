package runner_test

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/runner"
)

// limitMessage is what Claude Code prints when its account's usage limit is
// reached (ticket #271).
const limitMessage = "You've hit your 5-hour limit · resets 10pm (Europe/Berlin)"

func TestARunCutShortByTheUsageLimitReleasesItsTaskWithoutSpendingAnAttempt(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{},
		harness.Event{Kind: harness.EventMessage, Text: limitMessage},
		harness.Event{Kind: harness.EventUsageLimit, Text: limitMessage})
	_, task := f.readyTask(t, 1)
	f.start(t)
	s := f.session(t)
	s.finish <- harness.Result{ExitCode: 1}
	if released := f.waitEvent(t, "task.released"); released.Task != task.ID {
		t.Fatalf("task.released is of %q, want %q", released.Task, task.ID)
	}
	if got, _ := f.tasks.Get(ctx, task.ID); got.Attempts != 0 {
		t.Fatalf("attempts = %d after a run cut short by the usage limit, want 0", got.Attempts)
	}
	if failed := f.events(t, "task.failed"); len(failed) != 0 {
		t.Fatalf("task.failed = %s, want none", failed[0].Payload)
	}
	run, err := f.runner.Runs().Get(ctx, s.spec.Env["ISTARI_RUN"])
	if err != nil {
		t.Fatal(err)
	}
	if want := "the harness hit its usage limit: " + limitMessage; run.Reason != want {
		t.Fatalf("run reason = %q, want %q", run.Reason, want)
	}
}

func TestARunThatWorkedBeforeTheUsageLimitFailsAsBefore(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{},
		harness.Event{Kind: harness.EventToolCall, Tool: "Edit"},
		harness.Event{Kind: harness.EventUsageLimit, Text: limitMessage})
	_, task := f.readyTask(t, 2)
	f.start(t)
	f.session(t).finish <- harness.Result{ExitCode: 1}
	if failed := f.waitEvent(t, "task.failed"); !strings.Contains(string(failed.Payload), "without istari complete") || failed.Task != task.ID {
		t.Fatalf("task.failed = %s, want the exit without istari complete", failed.Payload)
	}
}

// limitedRunner is a runner whose harness hits the usage limit at once in
// every run, naming reset (zero: no reset time).
func limitedRunner(t *testing.T, reset time.Time) *runFixture {
	t.Helper()
	return newRunner(t, runner.RunnerOptions{},
		harness.Event{Kind: harness.EventMessage, Text: limitMessage},
		harness.Event{Kind: harness.EventUsageLimit, Text: limitMessage, Reset: reset})
}

func (f *runFixture) claims(t *testing.T) int {
	t.Helper()
	return len(f.events(t, "task.claimed"))
}

func TestTheUsageLimitPausesTheHandOutUntilTheResetAndThenTheTaskRuns(t *testing.T) {
	// Not parallel: sets the output of the process's standard logger.
	captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		reset := time.Now().Add(2 * time.Hour)
		f := limitedRunner(t, reset)
		_, task := f.readyTask(t, 1)
		f.start(t)
		f.session(t).finish <- harness.Result{ExitCode: 1}
		synctest.Wait()
		paused := f.runner.Paused()
		if len(paused) != 1 || paused[0].Slot != "implementer-1" || !strings.Contains(paused[0].Error, "usage limit") || !strings.Contains(paused[0].Error, limitMessage) {
			t.Fatalf("paused = %+v, want implementer-1 with the usage limit and %q", paused, limitMessage)
		}
		if !paused[0].Until.Equal(reset) {
			t.Fatalf("until = %s, want the reset %s", paused[0].Until, reset)
		}
		limited := f.events(t, "harness.limited")
		if len(limited) != 1 {
			t.Fatalf("%d harness.limited events, want 1", len(limited))
		}
		var payload struct {
			Harness, Run, Message string
			Until                 time.Time
		}
		if err := json.Unmarshal(limited[0].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Harness != f.h.name || payload.Message != limitMessage || !payload.Until.Equal(reset) || payload.Run == "" {
			t.Fatalf("harness.limited = %s, want harness %s, the message, a run and until %s", limited[0].Payload, f.h.name, reset)
		}

		after(time.Until(reset) - time.Second)
		if n := f.claims(t); n != 1 {
			t.Fatalf("%d claims before the reset, want 1", n)
		}
		if got, _ := f.tasks.Get(ctx, task.ID); got.State != core.TaskReady || got.Attempts != 0 {
			t.Fatalf("task is %s with %d attempts during the pause, want ready with 0", got.State, got.Attempts)
		}
		after(2 * time.Second)
		if s := f.session(t); s.spec.Env["ISTARI_TASK"] != task.ID {
			t.Fatalf("the run after the reset is of task %q, want %q", s.spec.Env["ISTARI_TASK"], task.ID)
		}
		if paused := f.runner.Paused(); len(paused) != 0 {
			t.Fatalf("paused = %+v after the reset, want none", paused)
		}
	})
}

func TestAUsageLimitWithoutAResetTimePausesForABackoffThatDoublesOnTheNextHit(t *testing.T) {
	// Not parallel: sets the output of the process's standard logger.
	captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		f := limitedRunner(t, time.Time{})
		f.readyTask(t, 1)
		f.start(t)
		for i, backoff := range []time.Duration{runner.DefaultLimitBackoff, 2 * runner.DefaultLimitBackoff} {
			f.session(t).finish <- harness.Result{ExitCode: 1}
			synctest.Wait()
			if paused := f.runner.Paused(); len(paused) != 1 || !paused[0].Until.Equal(time.Now().Add(backoff)) {
				t.Fatalf("hit %d: paused = %+v, want until %s", i+1, paused, time.Now().Add(backoff))
			}
			after(backoff - time.Second)
			if n := f.claims(t); n != i+1 {
				t.Fatalf("hit %d: %d claims during the backoff of %s, want %d", i+1, n, backoff, i+1)
			}
			after(2 * time.Second)
			if n := f.claims(t); n != i+2 {
				t.Fatalf("hit %d: %d claims after the backoff of %s, want %d", i+1, n, backoff, i+2)
			}
		}
	})
}
