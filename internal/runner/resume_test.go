package runner_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestTheRunnerKnowsWhichTaskItIsWorkingOn(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	_, task := f.readyTask(t, 9)
	if _, ok := f.runner.Working(ctx, task.ID); ok {
		t.Fatal("working on a task before any run")
	}
	f.start(t)
	s := f.session(t)
	if run, ok := f.runner.Working(ctx, task.ID); !ok || run != s.spec.Env["ISTARI_RUN"] {
		t.Fatalf("Working = %q, %v during run %s", run, ok, s.spec.Env["ISTARI_RUN"])
	}
	if _, ok := f.runner.Working(ctx, "t_other"); ok {
		t.Fatal("working on every task during one run")
	}
	if _, err := f.runner.Runs().Complete(ctx, s.spec.Env["ISTARI_RUN"], "done"); err != nil {
		t.Fatal(err)
	}
	s.finish <- harness.Result{ExitCode: 0}
	f.waitTask(t, task.ID, core.TaskCompleted)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := f.runner.Working(ctx, task.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still working after the run ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAMergeThatCannotStartLeavesTheWorktreeAsItWas(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 10, "Edit the readme")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.Dir, "README.md"), "uncommitted work\n") // a killed run left it
	f.moveMain(t, map[string]string{"README.md": "web, by main\n"})
	if conflicts, err := f.ws.MergeMain(ctx, tk.Ref); err == nil || len(conflicts) != 0 {
		t.Fatalf("MergeMain over uncommitted work = %v, %v; want an error", conflicts, err)
	}
	if body, _ := os.ReadFile(filepath.Join(w.Dir, "README.md")); string(body) != "uncommitted work\n" {
		t.Fatalf("README.md = %q: the uncommitted work is gone", body)
	}
}

func TestWhenTheRemoteCannotBeReachedTheJournalSaysMainWasNotMergedAndWhy(t *testing.T) {
	t.Parallel()
	f := newRunner(t, runner.RunnerOptions{})
	tk, task := f.readyTask(t, 12)
	if _, err := f.ws.Ensure(ctx, tk); err != nil {
		t.Fatal(err)
	}
	gitIn(t, filepath.Join(f.data, "repos", "acme", "web.git"), "remote", "set-url", "origin", filepath.Join(f.root, "nowhere.git"))
	f.start(t)
	f.waitEvent(t, journal.EventPrefix+string(journal.Note))
	ref, _ := tickets.ParseRef(tk.Ref)
	entries, err := journal.New(nil, f.store, nil).Stored(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind == journal.Note && e.ID == task.ID && strings.Contains(e.Text, "main branch was not merged") && strings.Contains(e.Text, "nowhere.git") {
			return
		}
	}
	t.Fatalf("journal = %+v; want a note that the main branch was not merged, and why", entries)
}

// failFirstFetch makes the remote refuse the clone's next fetch, and only
// that one, as a remote that is briefly out of reach would.
func (f *wsFixture) failFirstFetch(t *testing.T) {
	t.Helper()
	script := filepath.Join(f.root, "upload-pack")
	write(t, script, "#!/bin/sh\n"+
		"if [ -e \""+script+".failed\" ]; then exec git-upload-pack \"$@\"; fi\n"+
		"touch \""+script+".failed\"\n"+
		"echo 'the remote is out of reach' >&2\nexit 1\n")
	gitIn(t, filepath.Join(f.data, "repos", "acme", "web.git"), "config", "remote.origin.uploadpack", script)
}

func TestAFetchThatFailsOnceIsTriedAgainBeforeTheMerge(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 11, "Merge despite a blip")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	f.moveMain(t, map[string]string{"other.go": "package other\n"})
	f.failFirstFetch(t)
	if conflicts, err := f.ws.MergeMain(ctx, tk.Ref); err != nil || len(conflicts) != 0 {
		t.Fatalf("MergeMain after one failed fetch = %v, %v", conflicts, err)
	}
	if !exists(filepath.Join(w.Dir, "other.go")) {
		t.Fatal("main's new file is not in the worktree")
	}
}
