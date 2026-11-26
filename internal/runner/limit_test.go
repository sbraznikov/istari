package runner_test

import (
	"strings"
	"testing"

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
