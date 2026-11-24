package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
)

// checkTimeout bounds one check command.
const checkTimeout = 10 * time.Minute

// Checkpoints makes checkpoints (spec sections 4.6 and 7.7): a commit, a
// push to the ticket branch and a journal entry, with the checks run by code.
// What an agent says about its tests is not taken on trust.
type Checkpoints struct {
	ws      *Workspaces
	journal *journal.Journal
	tokens  vcs.Tokens // nil: pushes carry no token (a local remote)
}

// NewCheckpoints returns the checkpoint maker.
func NewCheckpoints(ws *Workspaces, j *journal.Journal, tokens vcs.Tokens) *Checkpoints {
	return &Checkpoints{ws: ws, journal: j, tokens: tokens}
}

// Make makes a checkpoint of a run's worktree. message is "<done>; <next>".
// Leftover changes are committed (the hook adds the trailer); the typecheck
// and test commands of the main branch's .istari/pipeline.toml run; with
// verify_red the slice's new tests run on the previous checkpoint's code and
// must fail; the branch is pushed; the entry records the commit, the real
// results, the changed files and what code found.
func (c *Checkpoints) Make(ctx context.Context, run Run, message string) (journal.Entry, error) {
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return journal.Entry{}, err
	}
	ws, err := c.ws.Get(ctx, run.Ticket)
	if err != nil {
		return journal.Entry{}, err
	}
	if ws.Branch == "" || !isWorktree(ctx, run.Dir) {
		return journal.Entry{}, fmt.Errorf("checkpoint: %s has no worktree", run.Ticket)
	}
	done, next, _ := strings.Cut(message, ";")
	entry := &journal.CheckpointEntry{Done: strings.TrimSpace(done), Next: strings.TrimSpace(next), Branch: ws.Branch, Checks: map[string]string{}}
	if entry.Done == "" {
		entry.Done = "checkpoint"
	}
	prev, err := c.previous(ctx, run, ref, ws)
	if err != nil {
		return journal.Entry{}, err
	}
	if err := commitLeftovers(ctx, run, entry.Done); err != nil {
		return journal.Entry{}, err
	}
	head, err := gitOut(ctx, run.Dir, "rev-parse", "HEAD")
	if err != nil {
		return journal.Entry{}, err
	}
	if head == prev {
		return journal.Entry{}, &core.Error{Code: core.CodeInvalidState, Message: "nothing changed since the last checkpoint"}
	}
	entry.Commit = head[:min(12, len(head))]
	var tests, gone []string // the slice's tests, as they are now; the test files it removed
	if out, err := gitOut(ctx, run.Dir, "diff", "--name-status", "-M", prev, head); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Split(line, "\t")
			status, paths := fields[0], fields[1:]
			entry.Files = append(entry.Files, paths[len(paths)-1])
			switch {
			case status == "D":
				if isTestFile(paths[0]) {
					gone = append(gone, paths[0])
				}
			case strings.HasPrefix(status, "R") && isTestFile(paths[0]):
				gone = append(gone, paths[0])
			}
			if status != "D" && isTestFile(paths[len(paths)-1]) {
				tests = append(tests, paths[len(paths)-1])
			}
		}
		slices.Sort(entry.Files)
	}
	if len(tests) == 0 {
		entry.Notes = append(entry.Notes, "no test change in this slice")
	}

	checks, verifyRed, err := c.checks(ctx, ws.Repo)
	if err != nil {
		return journal.Entry{}, err
	}
	for _, name := range []string{"typecheck", "test"} {
		cmd, ok := checks[name]
		if !ok {
			entry.Notes = append(entry.Notes, name+": no command in the main branch's "+pipeline.Path)
			continue
		}
		if passed, output := runCheck(ctx, run.Dir, cmd); passed {
			entry.Checks[name] = "green"
		} else {
			entry.Checks[name], entry.Red = "red", true
			if entry.Failures == nil {
				entry.Failures = map[string]string{}
			}
			entry.Failures[name] = output
		}
	}
	if cmd, ok := checks["test"]; ok && verifyRed && len(tests) > 0 {
		judged, passed, err := c.onPrevious(ctx, ws.Repo, run.Dir, prev, tests, gone, cmd)
		switch {
		case err != nil:
			return journal.Entry{}, err
		case !judged:
			entry.Notes = append(entry.Notes, "verify_red: the tests already fail on the code of the previous checkpoint, so red first cannot be judged")
		case passed:
			entry.Checks["verify_red"], entry.Red = "red", true
			entry.Notes = append(entry.Notes, "verify_red: the new tests ("+strings.Join(tests, ", ")+") pass on the code of the previous checkpoint: the slice is not counted")
		default:
			entry.Checks["verify_red"] = "green"
		}
	}

	url, private := c.ws.PushTarget(ws.Repo)
	var token string
	if c.tokens != nil {
		if token, err = c.tokens.Token(ctx, ws.Repo); err != nil {
			return journal.Entry{}, err
		}
	}
	if err := vcs.Push(ctx, vcs.PushSpec{Worktree: run.Dir, URL: url, Private: private, Own: ws.Branch, Target: ws.Branch, Force: true, Token: token}); err != nil {
		return journal.Entry{}, err
	}
	e := journal.Entry{Kind: journal.Checkpoint, Actor: run.Agent(), Checkpoint: entry}
	if err := c.journal.Append(ctx, ref, e); err != nil {
		return journal.Entry{}, err
	}
	return e, nil
}

// previous returns the commit of the ticket's last checkpoint, or where its
// branch left the main branch.
func (c *Checkpoints) previous(ctx context.Context, run Run, ref tickets.Ref, ws Workspace) (string, error) {
	entries, err := c.journal.Stored(ctx, ref)
	if err != nil {
		return "", err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if cp := entries[i].Checkpoint; cp != nil && cp.Commit != "" {
			if full, err := gitOut(ctx, run.Dir, "rev-parse", "--verify", "-q", cp.Commit+"^{commit}"); err == nil {
				return full, nil
			}
		}
	}
	main, err := mainBranch(ctx, c.ws.clonePath(ws.Repo))
	if err != nil {
		return "", err
	}
	return gitOut(ctx, run.Dir, "merge-base", "HEAD", "refs/remotes/origin/"+main)
}

// checks reads the check commands from the main branch: the ticket's branch
// must not choose the checks that judge it.
func (c *Checkpoints) checks(ctx context.Context, repo string) (map[string]string, bool, error) {
	data, err := c.ws.MainFile(ctx, repo, pipeline.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	p, err := pipeline.Parse(data)
	if err != nil {
		return nil, false, err
	}
	out := map[string]string{}
	verifyRed := true
	for k, v := range p.Checks {
		switch val := v.(type) {
		case string:
			out[k] = val
		case bool:
			if k == "verify_red" {
				verifyRed = val
			}
		}
	}
	return out, verifyRed, nil
}

// onPrevious runs the test command on the previous checkpoint's code in a
// scratch checkout: first as it is, which must pass for red first to be
// judged; then with the slice's tests in place, which must fail. It reports
// whether it could judge, and whether the slice's tests passed there.
func (c *Checkpoints) onPrevious(ctx context.Context, repo, dir, prev string, tests, gone []string, cmd string) (judged, passed bool, err error) {
	scratch, err := os.MkdirTemp("", "istari-red-")
	if err != nil {
		return false, false, err
	}
	os.Remove(scratch)
	clone := c.ws.clonePath(repo)
	c.ws.mu.Lock()
	_, err = gitOut(ctx, clone, "worktree", "add", "--detach", scratch, prev)
	c.ws.mu.Unlock()
	if err != nil {
		return false, false, err
	}
	defer func() {
		c.ws.mu.Lock()
		defer c.ws.mu.Unlock()
		gitOut(context.WithoutCancel(ctx), clone, "worktree", "remove", "--force", scratch)
		os.RemoveAll(scratch)
	}()
	if ok, _ := runCheck(ctx, scratch, cmd); !ok {
		return false, false, nil
	}
	for _, t := range gone {
		os.Remove(filepath.Join(scratch, t))
	}
	for _, t := range tests {
		data, err := os.ReadFile(filepath.Join(dir, t))
		if err != nil {
			return false, false, err
		}
		target := filepath.Join(scratch, t)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, false, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return false, false, err
		}
	}
	ok, _ := runCheck(ctx, scratch, cmd)
	return true, ok, nil
}

// commitLeftovers commits what the agent left uncommitted.
func commitLeftovers(ctx context.Context, run Run, message string) error {
	status, err := gitOut(ctx, run.Dir, "status", "--porcelain")
	if err != nil || status == "" {
		return err
	}
	if _, err := gitOut(ctx, run.Dir, "add", "-A"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "commit", "-q", "-m", message)
	cmd.Dir = run.Dir
	who := run.Profile + " (istari)"
	cmd.Env = append(checkEnv(), "GIT_AUTHOR_NAME="+who, "GIT_AUTHOR_EMAIL=istari@localhost", "GIT_COMMITTER_NAME="+who, "GIT_COMMITTER_EMAIL=istari@localhost")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runCheck runs a check command in dir and tells whether it passed, with
// the end of its output. The agent wrote the code it runs, so it runs without
// the daemon's secrets, and its whole process group ends at the timeout.
func runCheck(ctx context.Context, dir, command string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = checkEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	tail := string(out)
	if len(tail) > 1500 {
		tail = "…" + tail[len(tail)-1500:]
	}
	if ctx.Err() != nil {
		tail += "\n[stopped after " + checkTimeout.String() + "]"
	}
	return err == nil, strings.TrimSpace(tail)
}

// checkEnv is the daemon's environment without its secrets: tokens, keys,
// passwords and istari's own variables.
func checkEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		secret := strings.HasPrefix(upper, "ISTARI_") || strings.HasPrefix(upper, "SLACK_") || strings.HasPrefix(upper, "ANTHROPIC_") ||
			strings.HasPrefix(upper, "GH_") || strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "GIT_CONFIG_")
		for _, part := range []string{"TOKEN", "SECRET", "PASSWORD", "API_KEY", "PRIVATE_KEY", "CREDENTIAL"} {
			secret = secret || strings.Contains(upper, part)
		}
		if !secret {
			env = append(env, kv)
		}
	}
	return env
}

// isTestFile tells whether a path is a test by the usual conventions.
func isTestFile(p string) bool {
	base := path.Base(p)
	for _, part := range strings.Split(path.Dir(p), "/") {
		if part == "test" || part == "tests" || part == "__tests__" || part == "spec" {
			return true
		}
	}
	return strings.Contains(base, "_test.") || strings.HasPrefix(base, "test_") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.Contains(base, "_spec.")
}
