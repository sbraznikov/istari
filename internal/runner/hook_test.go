package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/runner"
)

// gitIn runs git in dir with a clean configuration and returns its output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// repoWithWorktree makes a repository with one commit, a commit-msg hook of
// its own that appends a line, and a linked worktree on a ticket branch.
func repoWithWorktree(t *testing.T) (main, worktree string) {
	t.Helper()
	root := t.TempDir()
	main = filepath.Join(root, "main")
	worktree = filepath.Join(root, "wt")
	gitIn(t, root, "init", "-q", "-b", "main", main)
	gitIn(t, main, "commit", "-q", "--allow-empty", "-m", "initial")
	own := "#!/bin/sh\necho 'Reviewed-by: the repo hook' >> \"$1\"\n"
	if err := os.WriteFile(filepath.Join(main, ".git", "hooks", "commit-msg"), []byte(own), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := "#!/bin/sh\necho ran >> " + filepath.Join(root, "pre-commit.log") + "\n"
	if err := os.WriteFile(filepath.Join(main, ".git", "hooks", "pre-commit"), []byte(pre), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, main, "worktree", "add", "-q", "-b", "istari/7/x", worktree)
	return main, worktree
}

func lastMessage(t *testing.T, dir string) string {
	t.Helper()
	return gitIn(t, dir, "log", "-1", "--format=%B")
}

func TestACommitInATicketWorktreeGetsTheTicketTrailerOnce(t *testing.T) {
	main, wt := repoWithWorktree(t)
	if err := runner.InstallTicketHook(context.Background(), wt, "gh:acme/web#7"); err != nil {
		t.Fatalf("InstallTicketHook: %v", err)
	}
	gitIn(t, wt, "commit", "-q", "--allow-empty", "-m", "Add rate limits")
	msg := lastMessage(t, wt)
	if strings.Count(msg, "Ticket: gh:acme/web#7") != 1 {
		t.Fatalf("message = %q, want the trailer once", msg)
	}
	if !strings.Contains(msg, "Reviewed-by: the repo hook") {
		t.Fatalf("message = %q: the repository's own commit-msg hook did not run", msg)
	}

	if log, _ := os.ReadFile(filepath.Join(filepath.Dir(wt), "pre-commit.log")); string(log) != "ran\n" {
		t.Fatalf("pre-commit log = %q: the repository's own pre-commit hook did not run in the worktree", log)
	}

	gitIn(t, wt, "commit", "-q", "--allow-empty", "-m", "Fix it\n\nTicket: gh:acme/web#7")
	if msg := lastMessage(t, wt); strings.Count(msg, "Ticket: gh:acme/web#7") != 1 {
		t.Fatalf("message = %q, want an existing trailer not repeated", msg)
	}

	gitIn(t, main, "commit", "-q", "--allow-empty", "-m", "Human work")
	if msg := lastMessage(t, main); strings.Contains(msg, "Ticket:") || !strings.Contains(msg, "Reviewed-by: the repo hook") {
		t.Fatalf("message in the main checkout = %q, want no trailer and the repo hook", msg)
	}
}
