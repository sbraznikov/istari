package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
)

var ctx = context.Background()

type wsFixture struct {
	root     string // holds the remotes
	storeDir string // holds istari.db
	data     string
	store    *store.SQLite
	tickets  *core.Tickets
	ws       *runner.Workspaces
}

// newWorkspaces serves acme/web from a local bare repository with one
// commit on main, as GitHub would.
func newWorkspaces(t *testing.T) *wsFixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remotes", "acme", "web.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", remote)
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "clone", "-q", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "initial")
	gitIn(t, seed, "push", "-q", "origin", "main")
	storeDir := t.TempDir()
	s, err := store.Open(filepath.Join(storeDir, "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	clock := func() time.Time { return time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC) }
	data := filepath.Join(root, "data")
	ws := runner.NewWorkspaces(s, runner.WorkspaceOptions{
		DataDir: data,
		Remote:  func(repo string) string { return "file://" + filepath.Join(root, "remotes", repo+".git") },
		Clock:   clock,
	})
	return &wsFixture{root: root, storeDir: storeDir, data: data, store: s, tickets: core.NewTickets(s, core.TicketOptions{Clock: clock}), ws: ws}
}

func (f *wsFixture) ticket(t *testing.T, n int, title string) core.Ticket {
	t.Helper()
	ref := "gh:acme/web#" + strconv.Itoa(n)
	tk, err := f.tickets.CreateWith(ctx, "human:alex", core.NewTicket{Ref: ref, Project: "web", Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestEnsureCreatesTheTicketsWorktreeOnItsBranchFromMain(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 123, "Add rate limits")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := filepath.Join(f.data, "workspaces", "web", "web", "123"); w.Dir != want {
		t.Fatalf("dir = %s, want %s", w.Dir, want)
	}
	if got := strings.TrimSpace(gitIn(t, w.Dir, "branch", "--show-current")); got != "istari/123/add-rate-limits" || w.Branch != got {
		t.Fatalf("branch = %q (workspace says %q), want istari/123/add-rate-limits", got, w.Branch)
	}
	if _, err := os.Stat(filepath.Join(w.Dir, "README.md")); err != nil {
		t.Fatalf("the worktree lacks main's files: %v", err)
	}
	gitIn(t, w.Dir, "commit", "-q", "--allow-empty", "-m", "Work")
	if msg := lastMessage(t, w.Dir); !strings.Contains(msg, "Ticket: gh:acme/web#123") {
		t.Fatalf("a commit in the worktree = %q, want the ticket trailer", msg)
	}
}

func (f *wsFixture) events(t *testing.T, typ string) []store.Event {
	t.Helper()
	events, err := f.store.Read(ctx, "", store.Filter{TypePrefix: typ}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestEveryStageReusesTheWorktreeAndTwoTicketsHaveTheirOwn(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	a, b := f.ticket(t, 1, "First"), f.ticket(t, 2, "Second")
	wa, err := f.ws.Ensure(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wa.Dir, "work.txt"), []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := f.ws.Ensure(ctx, a)
	if err != nil || again.Dir != wa.Dir {
		t.Fatalf("second Ensure = %s, %v; want %s", again.Dir, err, wa.Dir)
	}
	if _, err := os.Stat(filepath.Join(wa.Dir, "work.txt")); err != nil {
		t.Fatalf("the second Ensure lost the work in progress: %v", err)
	}
	wb, err := f.ws.Ensure(ctx, b)
	if err != nil || wb.Dir == wa.Dir || wb.Branch == wa.Branch {
		t.Fatalf("ticket 2 = %+v, %v; want its own directory and branch", wb, err)
	}
	if n := len(f.events(t, runner.EventWorkspaceCreated)); n != 2 {
		t.Fatalf("workspace.created events = %d, want 2", n)
	}
}

func TestAWorktreeDeletedByHandIsRecreatedFromTheRemoteBranch(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 7, "Add limits")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.Dir, "commit", "-q", "--allow-empty", "-m", "Checkpoint")
	gitIn(t, w.Dir, "push", "-q", "origin", "HEAD:refs/heads/"+w.Branch) // what a checkpoint pushes
	pushed := strings.TrimSpace(gitIn(t, w.Dir, "rev-parse", "HEAD"))
	if err := os.RemoveAll(w.Dir); err != nil {
		t.Fatal(err)
	}
	// The local branch is gone too: as if the data directory were new.
	clone := filepath.Join(f.data, "repos", "acme", "web.git")
	gitIn(t, clone, "worktree", "prune")
	gitIn(t, clone, "branch", "-D", w.Branch)

	again, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatalf("Ensure after deletion: %v", err)
	}
	if got := strings.TrimSpace(gitIn(t, again.Dir, "rev-parse", "HEAD")); got != pushed || again.Branch != w.Branch {
		t.Fatalf("recreated at %s on %s, want %s on %s", got, again.Branch, pushed, w.Branch)
	}
}

func TestWithIstarisDatabaseLostTheTicketBranchIsFoundOnTheRemote(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	w, err := f.ws.Ensure(ctx, f.ticket(t, 7, "Add limits"))
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.Dir, "push", "-q", "origin", "HEAD:refs/heads/"+w.Branch)
	if err := os.RemoveAll(f.data); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	ws := runner.NewWorkspaces(fresh, runner.WorkspaceOptions{
		DataDir: f.data,
		Remote:  func(repo string) string { return "file://" + filepath.Join(f.root, "remotes", repo+".git") },
	})
	renamed := core.Ticket{Ref: "gh:acme/web#7", Project: "web", Title: "Rate limits, renamed"}
	again, err := ws.Ensure(ctx, renamed)
	if err != nil || again.Branch != w.Branch {
		t.Fatalf("Ensure with a fresh database = %q, %v; want the remote branch %q", again.Branch, err, w.Branch)
	}
}

func TestRemoveDeletesACleanWorktreeAndItsLocalBranch(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 3, "Pushed work")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.Dir, "commit", "-q", "--allow-empty", "-m", "Done")
	gitIn(t, w.Dir, "push", "-q", "origin", "HEAD:refs/heads/"+w.Branch)
	got, err := f.ws.Remove(ctx, tk)
	if err != nil || got.State != runner.WorkspaceRemoved {
		t.Fatalf("Remove = %+v, %v; want removed", got, err)
	}
	if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
		t.Fatalf("the directory is still there: %v", err)
	}
	clone := filepath.Join(f.data, "repos", "acme", "web.git")
	if out := gitIn(t, clone, "branch", "--list", w.Branch); strings.TrimSpace(out) != "" {
		t.Fatalf("the local branch is still there: %q", out)
	}
	if n := len(f.events(t, runner.EventWorkspaceRemoved)); n != 1 {
		t.Fatalf("workspace.removed events = %d, want 1", n)
	}
}

func TestADirtyOrUnpushedWorktreeIsKept(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		make func(t *testing.T, dir string)
	}{
		{"uncommitted", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "draft.txt"), []byte("draft\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"unpushed", func(t *testing.T, dir string) {
			gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "Not pushed")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWorkspaces(t)
			tk := f.ticket(t, 4, "Half done")
			w, err := f.ws.Ensure(ctx, tk)
			if err != nil {
				t.Fatal(err)
			}
			tc.make(t, w.Dir)
			for range 2 {
				got, err := f.ws.Remove(ctx, tk)
				if err != nil || got.State != runner.WorkspaceKept || got.Reason == "" {
					t.Fatalf("Remove = %+v, %v; want kept with a reason", got, err)
				}
			}
			if _, err := os.Stat(w.Dir); err != nil {
				t.Fatalf("a %s worktree was removed: %v", tc.name, err)
			}
			if n := len(f.events(t, runner.EventWorkspaceKept)); n != 1 {
				t.Fatalf("workspace.kept events = %d, want 1 for two attempts", n)
			}
		})
	}
}

func (f *wsFixture) fire(t *testing.T, ref string, triggers ...string) {
	t.Helper()
	for _, tr := range triggers {
		if _, err := f.tickets.Fire(ctx, "system", ref, tr); err != nil {
			t.Fatalf("%s %s: %v", ref, tr, err)
		}
	}
}

func (f *wsFixture) reconcile(t *testing.T) {
	t.Helper()
	all, err := f.tickets.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ws.Reconcile(ctx, all); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (f *wsFixture) dir(n int) string {
	return filepath.Join(f.data, "workspaces", "web", "web", strconv.Itoa(n))
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestTheWorktreeFollowsTheTicketFromStartToMerge(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 5, "Follow me")
	f.fire(t, tk.Ref, "accept")
	f.reconcile(t)
	if exists(f.dir(5)) {
		t.Fatal("a ready ticket has a worktree; the refiner works without one")
	}
	f.fire(t, tk.Ref, "start") // T3
	f.reconcile(t)
	if !exists(f.dir(5)) {
		t.Fatal("T3 did not create the worktree")
	}
	f.fire(t, tk.Ref, "plan_accepted", "pr_ready", "review_passed", "merged") // … T11
	f.reconcile(t)
	if exists(f.dir(5)) {
		t.Fatal("T11 did not remove the worktree")
	}
}

func TestAParentTicketWithoutTasksOfItsOwnHasNoWorktree(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 6, "Too big")
	f.fire(t, tk.Ref, "accept", "start")
	f.reconcile(t)
	f.fire(t, tk.Ref, "split") // T5: the children do the work
	f.reconcile(t)
	if exists(f.dir(6)) {
		t.Fatal("a split parent kept its worktree")
	}
}

func TestACanceledTicketLosesItsWorktree(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 8, "Never mind")
	f.fire(t, tk.Ref, "accept", "start")
	f.reconcile(t)
	f.fire(t, tk.Ref, "cancel") // T13
	f.reconcile(t)
	if exists(f.dir(8)) {
		t.Fatal("T13 did not remove the worktree")
	}
}

func TestPruneRemovesOrphansOnly(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	active := f.ticket(t, 1, "Active")
	f.fire(t, active.Ref, "accept", "start")
	f.reconcile(t)
	// A worktree whose ticket istari does not know: left by a lost database.
	if _, err := f.ws.Ensure(ctx, core.Ticket{Ref: "gh:acme/web#99", Project: "web", Title: "Gone"}); err != nil {
		t.Fatal(err)
	}
	all, _ := f.tickets.List(ctx)
	orphans, err := f.ws.Orphans(ctx, all)
	if err != nil || len(orphans) != 1 || orphans[0].Ticket != "gh:acme/web#99" {
		t.Fatalf("Orphans = %+v, %v; want only #99", orphans, err)
	}
	pruned, err := f.ws.Prune(ctx, all)
	if err != nil || len(pruned) != 1 || pruned[0].Ticket != "gh:acme/web#99" {
		t.Fatalf("Prune = %+v, %v; want only #99", pruned, err)
	}
	if exists(f.dir(99)) || !exists(f.dir(1)) {
		t.Fatalf("after prune: #99 exists %v, #1 exists %v; want only #1", exists(f.dir(99)), exists(f.dir(1)))
	}
	list, err := f.ws.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v, %v; want both records", list, err)
	}
}

func TestAWorktreeLeftOnDiskWithoutARecordIsAnOrphanToo(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	w, err := f.ws.Ensure(ctx, f.ticket(t, 2, "Left behind"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Open(filepath.Join(t.TempDir(), "istari.db")) // istari's database is new
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	ws := runner.NewWorkspaces(fresh, runner.WorkspaceOptions{DataDir: f.data})
	orphans, err := ws.Orphans(ctx, nil)
	if err != nil || len(orphans) != 1 || orphans[0].Dir != w.Dir {
		t.Fatalf("Orphans = %+v, %v; want the directory %s", orphans, err, w.Dir)
	}
	if pruned, err := ws.Prune(ctx, nil); err != nil || len(pruned) != 1 || exists(w.Dir) {
		t.Fatalf("Prune = %+v, %v; the directory exists: %v", pruned, err, exists(w.Dir))
	}
}

func TestUnpushedCommitsOfAWorktreeDeletedByHandAreKept(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 9, "Only local")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.Dir, "commit", "-q", "--allow-empty", "-m", "Not pushed")
	if err := os.RemoveAll(w.Dir); err != nil {
		t.Fatal(err)
	}
	got, err := f.ws.Remove(ctx, tk)
	if err != nil || got.State != runner.WorkspaceKept {
		t.Fatalf("Remove = %+v, %v; want kept: the local branch holds the only copy", got, err)
	}
	clone := filepath.Join(f.data, "repos", "acme", "web.git")
	if out := gitIn(t, clone, "branch", "--list", w.Branch); strings.TrimSpace(out) == "" {
		t.Fatal("the local branch with unpushed commits was deleted")
	}
}

func TestRecreatingTakesTheRemoteBranchWhenItIsAhead(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 10, "Moved on")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.Dir, "push", "-q", "origin", "HEAD:refs/heads/"+w.Branch)
	if err := os.RemoveAll(w.Dir); err != nil {
		t.Fatal(err)
	}
	// Elsewhere (another machine) the branch moved on.
	other := filepath.Join(f.root, "other")
	gitIn(t, f.root, "clone", "-q", "-b", w.Branch, "file://"+filepath.Join(f.root, "remotes", "acme", "web.git"), other)
	gitIn(t, other, "commit", "-q", "--allow-empty", "-m", "Elsewhere")
	gitIn(t, other, "push", "-q", "origin", w.Branch)
	ahead := strings.TrimSpace(gitIn(t, other, "rev-parse", "HEAD"))

	again, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitIn(t, again.Dir, "rev-parse", "HEAD")); got != ahead {
		t.Fatalf("recreated at %s, want the remote's %s", got, ahead)
	}
}

func TestAKeptWorktreeIsUsedAgainWhenTheTicketIsReopened(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 11, "Back again")
	f.fire(t, tk.Ref, "accept", "start")
	f.reconcile(t)
	if err := os.WriteFile(filepath.Join(f.dir(11), "draft.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.fire(t, tk.Ref, "cancel")
	f.reconcile(t) // kept: it holds a draft
	f.fire(t, tk.Ref, "reopen", "accept", "start")
	f.reconcile(t)
	list, err := f.ws.List(ctx)
	if err != nil || len(list) != 1 || list[0].State != runner.WorkspaceActive {
		t.Fatalf("workspaces = %+v, %v; want the one worktree active again", list, err)
	}
	if !exists(filepath.Join(f.dir(11), "draft.txt")) {
		t.Fatal("the draft was lost")
	}
}

func TestAWorktreeWithoutARecordIsAdoptedByItsTicket(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	tk := f.ticket(t, 12, "Half made")
	w, err := f.ws.Ensure(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	// istari's database is new, as if it died before recording the worktree.
	fresh, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	ws := runner.NewWorkspaces(fresh, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	again, err := ws.Ensure(ctx, tk)
	if err != nil || again.Dir != w.Dir || again.Branch != w.Branch || again.State != runner.WorkspaceActive {
		t.Fatalf("Ensure over an unrecorded worktree = %+v, %v; want it adopted", again, err)
	}
}

func TestADirectoryThatIsNoWorktreeIsNoOrphan(t *testing.T) {
	t.Parallel()
	f := newWorkspaces(t)
	stray := filepath.Join(f.data, "workspaces", "web", "web", "notes")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if orphans, err := f.ws.Orphans(ctx, nil); err != nil || len(orphans) != 0 {
		t.Fatalf("Orphans = %+v, %v; a plain directory is not istari's", orphans, err)
	}
}
