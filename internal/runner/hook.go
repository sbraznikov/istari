package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sbraznikov/istari/internal/tickets"
)

// InstallTicketHook makes every commit in the ticket's worktree carry the
// trailer "Ticket: <ref>" (spec sections 2.1 and 7.7: added by a git hook, the
// agent does not need to remember it). The hook applies to this worktree only:
// it is set through the worktree's own core.hooksPath, so the human's main
// checkout is unchanged. The repository's own hooks keep running in the
// worktree: istari's hooks directory calls them.
func InstallTicketHook(ctx context.Context, worktree, ref string) error {
	if _, err := tickets.ParseRef(ref); err != nil {
		return err
	}
	// The repository's hooks as they apply now, before istari's take over.
	own, err := gitOut(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		return err
	}
	gitDir, err := gitOut(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return err
	}
	dir := filepath.Join(gitDir, "istari-hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(own)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "commit-msg" || strings.HasSuffix(name, ".sample") {
			continue
		}
		wrapper := fmt.Sprintf("#!/bin/sh\nexec %s \"$@\"\n", quote(filepath.Join(own, name)))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(wrapper), 0o755); err != nil {
			return err
		}
	}
	ownMsg := quote(filepath.Join(own, "commit-msg"))
	hook := fmt.Sprintf(`#!/bin/sh
# istari: every commit of this ticket carries its trailer.
git interpret-trailers --in-place --if-exists addIfDifferent --trailer %s "$1" || exit 1
if [ -x %s ]; then exec %s "$@"; fi
`, quote("Ticket: "+ref), ownMsg, ownMsg)
	if err := os.WriteFile(filepath.Join(dir, "commit-msg"), []byte(hook), 0o755); err != nil {
		return err
	}
	if _, err := gitOut(ctx, worktree, "config", "extensions.worktreeConfig", "true"); err != nil {
		return err
	}
	_, err = gitOut(ctx, worktree, "config", "--worktree", "core.hooksPath", dir)
	return err
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exit.Stderr))
		}
		return "", fmt.Errorf("git %s: %v %s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

// quote quotes s for sh.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
