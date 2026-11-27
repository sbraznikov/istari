package main_test

import (
	"context"
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

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
