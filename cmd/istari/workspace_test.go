package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/exectest"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitServer makes a bare acme/web with one commit on main under root, as
// GITHUB_SERVER_URL=file://root serves it.
func gitServer(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "acme", "web.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", remote)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", remote, seed)
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "initial")
	git(t, seed, "push", "-q", "origin", "main")
	return root
}

// withoutHarness returns path with a claude in front that fails its check:
// a daemon on it has no runner slot, so only the test claims tasks, on a
// machine with Claude Code installed as on one without.
func withoutHarness(t *testing.T, path string) string {
	t.Helper()
	bin := t.TempDir()
	exectest.Write(t, filepath.Join(bin, "claude"), "#!/bin/sh\nexit 1\n")
	return bin + string(os.PathListSeparator) + path
}

// startPipelineDaemon starts a stepped daemon whose project web runs
// acme/web through its pipeline, with ticket ref already in planning: the
// first pipeline step gives the ticket its plan task. It has no runner slot
// (withoutHarness). before runs on the store before the daemon starts;
// server is the git server of gitServer.
func startPipelineDaemon(t *testing.T, ref string, before func(s store.Store, server, dataDir string)) *daemon {
	t.Helper()
	server := gitServer(t)
	dataDir := newDataDir(t)
	s, err := store.Open(filepath.Join(dataDir, api.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	k := core.NewTickets(s, core.TicketOptions{})
	ctx := context.Background()
	if _, err := k.CreateWith(ctx, "human:alex", core.NewTicket{Ref: ref, Project: "web", Title: "Add limits"}); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []string{"accept", "start"} {
		if _, err := k.Fire(ctx, "system", ref, tr); err != nil {
			t.Fatal(err)
		}
	}
	if before != nil {
		before(s, server, dataDir)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	gh := githubtest.New(t) // the tracker of acme/web
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return startSteppedDaemon(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server, "PATH="+withoutHarness(t, fakeGH(t, gh.Token)), "GITHUB_API_URL="+gh.URL)
}

func TestTheDaemonKeepsTicketWorktreesAndPrunesOrphans(t *testing.T) {
	t.Parallel()
	// Before the daemon starts: a ticket in planning, and a worktree whose
	// ticket istari does not know.
	var orphan runner.Workspace
	d := startPipelineDaemon(t, "gh:acme/web#5", func(s store.Store, server, dataDir string) {
		remote := func(repo string) string { return "file://" + filepath.Join(server, repo+".git") }
		var err error
		orphan, err = runner.NewWorkspaces(s, runner.WorkspaceOptions{DataDir: dataDir, Remote: remote}).Ensure(context.Background(), core.Ticket{Ref: "gh:acme/web#99", Project: "web", Title: "Gone"})
		if err != nil {
			t.Fatal(err)
		}
	})
	d.step(t, "pipeline") // the plan task of #5, and its worktree
	dataDir := d.dataDir
	want := filepath.Join(dataDir, "workspaces", "web", "web", "5")
	var list []struct{ Ticket, State, Branch, Dir string }
	d.mustCLI(t, &list, "workspace", "list")
	if len(list) != 2 {
		t.Fatalf("workspace list = %+v, want #5 and the orphan\nstderr: %s", list, d.stderr)
	}
	if !exists(want) {
		t.Fatalf("the daemon did not create %s", want)
	}

	out, _, code := d.cli(t, "doctor")
	if code != 1 || !strings.Contains(out, "gh:acme/web#99") {
		t.Fatalf("doctor with an orphan: exit %d\n%s", code, out)
	}
	var pruned []struct{ Ticket string }
	d.mustCLI(t, &pruned, "workspace", "prune")
	if len(pruned) != 1 || pruned[0].Ticket != "gh:acme/web#99" || exists(orphan.Dir) || !exists(want) {
		t.Fatalf("prune = %+v; orphan exists %v, #5 exists %v", pruned, exists(orphan.Dir), exists(want))
	}
}

// doctorCheck is one check of doctor's --json output.
type doctorCheck struct {
	Name, Detail string
	OK, Warn     bool
}

// doctorRow returns doctor's check of that name from its --json output.
func doctorRow(t *testing.T, out, name string) (doctorCheck, bool) {
	t.Helper()
	var checks []doctorCheck
	if err := json.Unmarshal([]byte(out), &checks); err != nil {
		t.Fatalf("doctor --json: %v in %q", err, out)
	}
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return doctorCheck{}, false
}

func TestPruneAndDoctorSayWhyAWorktreeWithUnsavedWorkIsKept(t *testing.T) {
	t.Parallel()
	// Before the daemon starts: a worktree whose ticket istari does not know,
	// two done tickets whose removal kept their worktree, and a worktree with
	// uncommitted changes that istari has no record of.
	var orphan, unpushed, uncommitted, unrecorded runner.Workspace
	d := startPipelineDaemon(t, "gh:acme/web#5", func(s store.Store, server, dataDir string) {
		ctx := context.Background()
		remote := func(repo string) string { return "file://" + filepath.Join(server, repo+".git") }
		spaces := runner.NewWorkspaces(s, runner.WorkspaceOptions{DataDir: dataDir, Remote: remote})
		k := core.NewTickets(s, core.TicketOptions{})
		var err error
		if orphan, err = spaces.Ensure(ctx, core.Ticket{Ref: "gh:acme/web#99", Project: "web", Title: "Gone"}); err != nil {
			t.Fatal(err)
		}
		done := func(n int, unsaved func(dir string)) runner.Workspace {
			ref := fmt.Sprintf("gh:acme/web#%d", n)
			tk, err := k.CreateWith(ctx, "human:alex", core.NewTicket{Ref: ref, Project: "web", Title: "Done"})
			if err != nil {
				t.Fatal(err)
			}
			w, err := spaces.Ensure(ctx, tk)
			if err != nil {
				t.Fatal(err)
			}
			unsaved(w.Dir)
			for _, tr := range []string{"accept", "start", "plan_accepted", "pr_ready", "review_passed", "merged"} {
				if _, err := k.Fire(ctx, "system", ref, tr); err != nil {
					t.Fatal(err)
				}
			}
			if w, err = spaces.Remove(ctx, tk); err != nil || w.State != runner.WorkspaceKept {
				t.Fatalf("Remove %s = %+v, %v; want kept", ref, w, err)
			}
			return w
		}
		unpushed = done(164, func(dir string) { git(t, dir, "commit", "-q", "--allow-empty", "-m", "Not pushed") })
		draft := func(dir string) {
			if err := os.WriteFile(filepath.Join(dir, "draft.txt"), []byte("draft\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		uncommitted = done(259, draft)
		lost, err := store.Open(filepath.Join(t.TempDir(), api.DatabaseName)) // its record is in another database
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := lost.Close(); err != nil {
				t.Error(err)
			}
		}()
		other := runner.NewWorkspaces(lost, runner.WorkspaceOptions{DataDir: dataDir, Remote: remote})
		if unrecorded, err = other.Ensure(ctx, core.Ticket{Ref: "gh:acme/web#77", Project: "web", Title: "Lost"}); err != nil {
			t.Fatal(err)
		}
		draft(unrecorded.Dir)
	})

	out, _, _ := d.cli(t, "--json", "doctor")
	fail, ok := doctorRow(t, out, "workspaces")
	if !ok || fail.OK || fail.Warn || !strings.Contains(fail.Detail, "gh:acme/web#99") || !strings.Contains(fail.Detail, "istari workspace prune") || strings.Contains(fail.Detail, "#164") || strings.Contains(fail.Detail, "#259") || strings.Contains(fail.Detail, unrecorded.Dir) {
		t.Fatalf("doctor workspaces = %+v; want a failure for #99 alone, with the prune advice\n%s", fail, out)
	}
	kept, ok := doctorRow(t, out, "workspaces kept")
	if !ok || kept.OK || !kept.Warn || strings.Contains(kept.Detail, "prune") {
		t.Fatalf("doctor workspaces kept = %+v; want a warning without the prune advice\n%s", kept, out)
	}
	for _, w := range []string{"gh:acme/web#164 " + unpushed.Dir + ": 1 commits not on the remote", "gh:acme/web#259 " + uncommitted.Dir + ": uncommitted changes", "- " + unrecorded.Dir + ": uncommitted changes"} {
		if !strings.Contains(kept.Detail, w) {
			t.Fatalf("doctor workspaces kept = %q; want %q", kept.Detail, w)
		}
	}

	var pruned []runner.Workspace
	d.mustCLI(t, &pruned, "workspace", "prune")
	got := map[string]runner.Workspace{}
	for _, w := range pruned {
		got[w.Ticket] = w
	}
	if w := got["gh:acme/web#99"]; len(pruned) != 4 || w.State != runner.WorkspaceRemoved || exists(orphan.Dir) {
		t.Fatalf("prune = %+v; want #99 removed and #164, #259 and the unrecorded worktree kept", pruned)
	}
	unrecorded.Ticket, unrecorded.Reason = "", "uncommitted changes" // as prune sees it without a record
	for ref, want := range map[string]runner.Workspace{"gh:acme/web#164": unpushed, "gh:acme/web#259": uncommitted, "": unrecorded} {
		if w := got[ref]; w.State != runner.WorkspaceKept || w.Dir != want.Dir || w.Reason != want.Reason || !exists(want.Dir) {
			t.Fatalf("prune %s = %+v; want kept in %s for %q", ref, w, want.Dir, want.Reason)
		}
	}
	table, stderr, code := d.cli(t, "workspace", "prune")
	if code != 0 {
		t.Fatalf("workspace prune: exit %d: %s", code, stderr)
	}
	for _, w := range []runner.Workspace{unpushed, uncommitted} {
		if !strings.Contains(table, w.Ticket) || !strings.Contains(table, w.Dir) || !strings.Contains(table, w.Reason) {
			t.Fatalf("workspace prune shows no ticket, dir and reason of %s:\n%s", w.Ticket, table)
		}
	}

	out, _, _ = d.cli(t, "--json", "doctor")
	if fail, ok := doctorRow(t, out, "workspaces"); !ok || !fail.OK {
		t.Fatalf("doctor workspaces after prune = %+v; want ok\n%s", fail, out)
	}
	if kept, ok := doctorRow(t, out, "workspaces kept"); !ok || !kept.Warn {
		t.Fatalf("doctor workspaces kept after prune = %+v; want still a warning\n%s", kept, out)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
