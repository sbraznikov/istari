package runner_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// moveMain commits files to the remote's main branch, as another merged
// pull request would.
func (f *wsFixture) moveMain(t *testing.T, files map[string]string) {
	t.Helper()
	seed := filepath.Join(f.root, "seed")
	gitIn(t, seed, "pull", "-q", "origin", "main")
	for name, body := range files {
		write(t, filepath.Join(seed, name), body)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "main moved")
	gitIn(t, seed, "push", "-q", "origin", "main")
}

func TestResumingMergesTheMovedMainBranchIntoTheWorktree(t *testing.T) {
	f := newWorkspaces(t)
	tk := f.ticket(t, 5, "Add limits")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.Dir, "limits.go"), "package limits\n")
	gitIn(t, w.Dir, "add", ".")
	gitIn(t, w.Dir, "commit", "-q", "-m", "limits")
	f.moveMain(t, map[string]string{"other.go": "package other\n"})
	conflicts, err := f.ws.MergeMain(ctx, tk.Ref)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("MergeMain = %v, %v", conflicts, err)
	}
	if _, err := os.Stat(filepath.Join(w.Dir, "other.go")); err != nil {
		t.Fatalf("main's new file is not in the worktree: %v", err)
	}
	if msg := lastMessage(t, w.Dir); !strings.Contains(msg, "Ticket: "+tk.Ref) {
		t.Fatalf("the merge commit = %q, want the ticket trailer", msg)
	}
}

func TestConflictsWithTheMainBranchAreLeftForTheWorker(t *testing.T) {
	f := newWorkspaces(t)
	tk := f.ticket(t, 6, "Edit the readme")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.Dir, "README.md"), "web, by the ticket\n")
	gitIn(t, w.Dir, "commit", "-q", "-am", "readme")
	f.moveMain(t, map[string]string{"README.md": "web, by main\n"})
	conflicts, err := f.ws.MergeMain(ctx, tk.Ref)
	if err != nil || !slices.Equal(conflicts, []string{"README.md"}) {
		t.Fatalf("MergeMain = %v, %v; want the conflict in README.md", conflicts, err)
	}
	if again, err := f.ws.MergeMain(ctx, tk.Ref); err != nil || !slices.Equal(again, []string{"README.md"}) {
		t.Fatalf("MergeMain before the worker resolved them = %v, %v", again, err)
	}
	body, _ := os.ReadFile(filepath.Join(w.Dir, "README.md"))
	if !strings.Contains(string(body), "<<<<<<<") {
		t.Fatalf("README.md = %q, want the conflict markers left for the worker", body)
	}
}

func TestAfterAKilledRunTheNextOneStartsFromTheCheckpointAndTheConflicts(t *testing.T) {
	f := newRunner(t, runner.RunnerOptions{})
	tk, task := f.readyTask(t, 8)
	ref, _ := tickets.ParseRef(tk.Ref)
	f.start(t)
	s := f.session(t)
	write(t, filepath.Join(s.spec.Workdir, "README.md"), "web, by the ticket\n")
	gitIn(t, s.spec.Workdir, "commit", "-q", "-am", "readme")
	if err := journal.New(nil, f.store, nil).Append(ctx, ref, journal.Entry{Kind: journal.Checkpoint, Actor: "agent:implementer-1",
		Checkpoint: &journal.CheckpointEntry{Done: "the readme says who wrote it", Next: "TEACH THE CACHE TO EXPIRE"}}); err != nil {
		t.Fatal(err)
	}
	f.moveMain(t, map[string]string{"README.md": "web, by main\n"})
	s.finish <- harness.Result{ExitCode: -1} // the harness was killed
	f.waitEvent(t, "task.failed")
	again := f.session(t)
	if again.spec.Env["ISTARI_TASK"] != task.ID {
		t.Fatalf("the next run is of task %s, want %s", again.spec.Env["ISTARI_TASK"], task.ID)
	}
	b := again.spec.Brief
	first, next := strings.Index(b, "- README.md"), strings.Index(b, "TEACH THE CACHE TO EXPIRE")
	if first < 0 || next < 0 || first > next {
		t.Fatalf("brief of the next run:\n%s", b)
	}
	again.Stop(0)
}
