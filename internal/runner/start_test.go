package runner_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/runner"
)

// errMissing is how a harness binary that is being replaced fails to start.
var errMissing = errors.New("start claude: fork/exec /Users/pc/.local/bin/claude: no such file or directory")

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
