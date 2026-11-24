package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Event types of ticket workspaces.
const (
	EventWorkspaceCreated = "workspace.created"
	EventWorkspaceRemoved = "workspace.removed"
	EventWorkspaceKept    = "workspace.kept" // not removed: it holds work that is not pushed
)

const workspaceKind = "workspace"

// Workspace states.
const (
	WorkspaceActive  = "active"
	WorkspaceKept    = "kept"
	WorkspaceRemoved = "removed"
)

// Workspace is a ticket's git worktree (spec section 7.2).
type Workspace struct {
	Ticket  string    `json:"ticket"`
	Project string    `json:"project"`
	Repo    string    `json:"repo"`
	Dir     string    `json:"dir"`
	Branch  string    `json:"branch"`
	State   string    `json:"state"`            // active, kept or removed
	Reason  string    `json:"reason,omitempty"` // why a kept workspace was not removed
	Updated time.Time `json:"updated"`

	version int64
}

// WorkspaceOptions configures Workspaces.
type WorkspaceOptions struct {
	DataDir string
	// Server is the git server of the repositories; "" means
	// https://github.com.
	Server string
	// Remote returns the git URL of a repository (owner/repo); nil means
	// <Server>/<repo>.git.
	Remote func(repo string) string
	Clock  func() time.Time
}

// Workspaces keeps the worktrees of tickets. Every repository has one clone
// of istari's own in <data_dir>/repos, fetched from its remote; the worktrees
// are made from it, so the human's working copy is never used or changed.
type Workspaces struct {
	store store.Store
	opts  WorkspaceOptions
	mu    sync.Mutex // one git operation on the clones at a time
}

// NewWorkspaces returns the ticket workspaces.
func NewWorkspaces(s store.Store, opts WorkspaceOptions) *Workspaces {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Workspaces{store: s, opts: opts}
}

// Ensure returns the ticket's worktree, creating it when it does not exist:
// in <data_dir>/workspaces/<project>/<repo>/<ticket> on the ticket's branch.
// A branch that exists, locally or on the remote, is checked out again, so a
// lost worktree is recreated from the ticket branch; otherwise a new branch
// istari/<key>/<slug> starts from the main branch.
func (w *Workspaces) Ensure(ctx context.Context, tk core.Ticket) (Workspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ref, err := tickets.ParseRef(tk.Ref)
	if err != nil {
		return Workspace{}, err
	}
	if ref.Provider != "gh" {
		return Workspace{}, fmt.Errorf("workspace of %s: only GitHub repositories have workspaces so far", tk.Ref)
	}
	ws, err := w.get(ctx, tk.Ref)
	if err != nil {
		return Workspace{}, err
	}
	if ws.State == WorkspaceActive && isWorktree(ctx, ws.Dir) {
		return ws, nil
	}
	clone, err := w.clone(ctx, ref.Repo)
	if err != nil {
		return Workspace{}, err
	}
	ws.Ticket, ws.Repo = tk.Ref, ref.Repo
	ws.Project = tk.Project
	if ws.Project == "" {
		ws.Project = repoName(ref.Repo)
	}
	ws.Dir = filepath.Join(w.opts.DataDir, "workspaces", ws.Project, repoName(ref.Repo), strconv.Itoa(ref.Number))
	if ws.Branch == "" {
		if ws.Branch, err = remoteBranch(ctx, clone, ref); err != nil {
			return Workspace{}, err
		}
	}
	if ws.Branch == "" {
		ws.Branch = Branch(ref, tk.Title)
	}
	if _, err := gitOut(ctx, clone, "worktree", "prune"); err != nil {
		return Workspace{}, err
	}
	if err := os.MkdirAll(filepath.Dir(ws.Dir), 0o755); err != nil {
		return Workspace{}, err
	}
	var from string
	switch {
	case hasRef(ctx, clone, "refs/heads/"+ws.Branch):
		from = ws.Branch
		_, err = gitOut(ctx, clone, "worktree", "add", ws.Dir, ws.Branch)
	case hasRef(ctx, clone, "refs/remotes/origin/"+ws.Branch):
		from = "origin/" + ws.Branch
		_, err = gitOut(ctx, clone, "worktree", "add", "-b", ws.Branch, ws.Dir, from)
	default:
		main, merr := mainBranch(ctx, clone)
		if merr != nil {
			return Workspace{}, merr
		}
		from = "origin/" + main
		_, err = gitOut(ctx, clone, "worktree", "add", "-b", ws.Branch, ws.Dir, from)
	}
	if err != nil {
		return Workspace{}, err
	}
	if err := InstallTicketHook(ctx, ws.Dir, tk.Ref); err != nil {
		return Workspace{}, err
	}
	ws.State, ws.Reason = WorkspaceActive, ""
	if err := w.commit(ctx, &ws, EventWorkspaceCreated, map[string]any{"dir": ws.Dir, "branch": ws.Branch, "from": from}); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}

// Remove deletes the ticket's worktree and its local branch, at done and at
// canceled (spec section 7.2). A worktree with uncommitted changes or commits
// that are not on the remote is kept instead, with one workspace.kept event:
// losing work is worse than leaving a directory behind. The remote branch is
// not touched.
func (w *Workspaces) Remove(ctx context.Context, tk core.Ticket) (Workspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ws, err := w.get(ctx, tk.Ref)
	if err != nil || ws.Ticket == "" || ws.State == WorkspaceRemoved {
		return ws, err
	}
	clone := filepath.Join(w.opts.DataDir, "repos", ws.Repo+".git")
	if isWorktree(ctx, ws.Dir) {
		reason, err := unsaved(ctx, ws.Dir)
		if err != nil {
			return Workspace{}, err
		}
		if reason != "" {
			if ws.State == WorkspaceKept && ws.Reason == reason {
				return ws, nil
			}
			ws.State, ws.Reason = WorkspaceKept, reason
			return ws, w.commit(ctx, &ws, EventWorkspaceKept, map[string]any{"dir": ws.Dir, "reason": reason})
		}
		if _, err := gitOut(ctx, clone, "worktree", "remove", ws.Dir); err != nil {
			return Workspace{}, err
		}
	} else if err := os.RemoveAll(ws.Dir); err != nil {
		return Workspace{}, err
	}
	if _, err := gitOut(ctx, clone, "worktree", "prune"); err != nil {
		return Workspace{}, err
	}
	if hasRef(ctx, clone, "refs/heads/"+ws.Branch) {
		if _, err := gitOut(ctx, clone, "branch", "-D", ws.Branch); err != nil {
			return Workspace{}, err
		}
	}
	ws.State, ws.Reason = WorkspaceRemoved, ""
	return ws, w.commit(ctx, &ws, EventWorkspaceRemoved, map[string]any{"dir": ws.Dir, "branch": ws.Branch})
}

// unsaved says what in the worktree would be lost by removing it, or "".
func unsaved(ctx context.Context, dir string) (string, error) {
	status, err := gitOut(ctx, dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "uncommitted changes", nil
	}
	if _, err := gitOut(ctx, dir, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", err
	}
	// Commits are safe once the remote has them on some branch: the ticket
	// branch, or the main branch after a merge commit.
	out, err := gitOut(ctx, dir, "rev-list", "--count", "HEAD", "--not", "--remotes=origin")
	if err != nil {
		return "", err
	}
	if out != "0" {
		return out + " commits not on the remote", nil
	}
	return "", nil
}

// Reconcile gives every ticket the worktree its state calls for (spec
// section 7.2): from T3 until merge a ticket has one; a parent split into
// children (T5) has none of its own; done and canceled tickets lose theirs.
// Tickets before planning have none: the refiner reads the main branch. It
// is idempotent, so it also repairs what a restart or a hand left behind.
func (w *Workspaces) Reconcile(ctx context.Context, all []core.Ticket) error {
	var errs []error
	for _, tk := range all {
		if ref, err := tickets.ParseRef(tk.Ref); err != nil || ref.Provider != "gh" {
			continue
		}
		var err error
		switch tk.State {
		case core.TicketPlanning, core.TicketInProgress, core.TicketInReview, core.TicketAwaitingApproval:
			var parent bool
			if parent, err = w.split(ctx, tk.Ref); err == nil {
				if parent {
					_, err = w.Remove(ctx, tk)
				} else {
					_, err = w.Ensure(ctx, tk)
				}
			}
		case core.TicketDone, core.TicketCanceled:
			_, err = w.Remove(ctx, tk)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tk.Ref, err))
		}
	}
	return errors.Join(errs...)
}

// List returns every workspace istari has recorded, removed ones included.
func (w *Workspaces) List(ctx context.Context) ([]Workspace, error) {
	recs, err := w.store.List(ctx, workspaceKind)
	if err != nil {
		return nil, err
	}
	out := make([]Workspace, 0, len(recs))
	for _, rec := range recs {
		var ws Workspace
		if err := json.Unmarshal(rec.Data, &ws); err != nil {
			return nil, fmt.Errorf("workspace %s: %w", rec.ID, err)
		}
		ws.version = rec.Version
		out = append(out, ws)
	}
	return out, nil
}

// Orphans returns the worktrees that should not exist: of a ticket istari does
// not know, or of a done or canceled ticket that a removal left behind (spec
// section 7.2: istari doctor finds them).
func (w *Workspaces) Orphans(ctx context.Context, all []core.Ticket) ([]Workspace, error) {
	state := map[string]core.TicketState{}
	for _, tk := range all {
		state[tk.Ref] = tk.State
	}
	list, err := w.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Workspace
	recorded := map[string]bool{}
	for _, ws := range list {
		if ws.State == WorkspaceRemoved {
			continue
		}
		recorded[ws.Dir] = true
		st, known := state[ws.Ticket]
		if !known || st == core.TicketDone || st == core.TicketCanceled {
			out = append(out, ws)
		}
	}
	// Directories istari has no record of, e.g. after its database was lost.
	dirs, err := filepath.Glob(filepath.Join(w.opts.DataDir, "workspaces", "*", "*", "*"))
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if !recorded[dir] {
			out = append(out, Workspace{Dir: dir, State: WorkspaceActive, Reason: "no record"})
		}
	}
	return out, nil
}

// Prune removes the orphans and returns the ones it removed. An orphan that
// holds unsaved work is kept, as at any removal.
func (w *Workspaces) Prune(ctx context.Context, all []core.Ticket) ([]Workspace, error) {
	orphans, err := w.Orphans(ctx, all)
	if err != nil {
		return nil, err
	}
	var pruned []Workspace
	var errs []error
	for _, o := range orphans {
		var ws Workspace
		var err error
		if o.Ticket == "" {
			ws, err = w.removeUnrecorded(ctx, o)
		} else {
			ws, err = w.Remove(ctx, core.Ticket{Ref: o.Ticket})
		}
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", o.Ticket, err))
		case ws.State == WorkspaceRemoved:
			pruned = append(pruned, ws)
		}
	}
	return pruned, errors.Join(errs...)
}

// removeUnrecorded removes a worktree directory istari has no record of,
// unless it holds unsaved work. Its clone is the one git names for it.
func (w *Workspaces) removeUnrecorded(ctx context.Context, o Workspace) (Workspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !isWorktree(ctx, o.Dir) {
		return o, nil // not a worktree: not istari's to delete
	}
	reason, err := unsaved(ctx, o.Dir)
	if err != nil || reason != "" {
		o.State, o.Reason = WorkspaceKept, reason
		return o, err
	}
	clone, err := gitOut(ctx, o.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return o, err
	}
	if _, err := gitOut(ctx, clone, "worktree", "remove", o.Dir); err != nil {
		return o, err
	}
	o.State = WorkspaceRemoved
	return o, nil
}

// split tells whether the ticket was split into children (T5) since it was
// last started.
func (w *Workspaces) split(ctx context.Context, ref string) (bool, error) {
	events, err := w.store.Read(ctx, "", store.Filter{Ticket: ref, TypePrefix: core.EventTicketTransitioned}, 0)
	if err != nil {
		return false, err
	}
	split := false
	for _, e := range events {
		var p struct {
			Trigger string `json:"trigger"`
		}
		if json.Unmarshal(e.Payload, &p) == nil {
			switch p.Trigger {
			case "split":
				split = true
			case "start":
				split = false
			}
		}
	}
	return split, nil
}

// get returns the ticket's workspace record, or a zero one.
func (w *Workspaces) get(ctx context.Context, ref string) (Workspace, error) {
	rec, err := w.store.Get(ctx, workspaceKind, ref)
	if errors.Is(err, store.ErrNotFound) {
		return Workspace{}, nil
	}
	if err != nil {
		return Workspace{}, err
	}
	var ws Workspace
	if err := json.Unmarshal(rec.Data, &ws); err != nil {
		return Workspace{}, fmt.Errorf("workspace %s: %w", ref, err)
	}
	ws.version = rec.Version
	return ws, nil
}

func (w *Workspaces) commit(ctx context.Context, ws *Workspace, typ string, payload map[string]any) error {
	ws.Updated = w.opts.Clock().UTC()
	data, _ := json.Marshal(ws)
	raw, _ := json.Marshal(payload)
	_, err := w.store.Append(ctx, store.Event{Type: typ, At: ws.Updated, Actor: "system", Project: ws.Project, Ticket: ws.Ticket, Payload: raw},
		store.Change{Kind: workspaceKind, ID: ws.Ticket, Data: data, IfVersion: ws.version})
	if err == nil {
		ws.version++
	}
	return err
}

// clone returns istari's clone of repo, fetched from its remote just now.
// Remote branches are kept as origin/*, so fetching never moves a ticket's
// local branch.
func (w *Workspaces) clone(ctx context.Context, repo string) (string, error) {
	dir := filepath.Join(w.opts.DataDir, "repos", repo+".git")
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if _, err := gitOut(ctx, filepath.Dir(dir), "init", "-q", "--bare", dir); err != nil {
			return "", err
		}
		// The ticket hooks use per-worktree config; with it, core.bare must
		// live in the clone's own worktree config, or every worktree would be
		// taken for bare too.
		for _, args := range [][]string{
			{"config", "extensions.worktreeConfig", "true"},
			{"config", "--unset", "core.bare"},
			{"config", "--worktree", "core.bare", "true"},
			{"remote", "add", "origin", w.remote(repo)},
		} {
			if _, err := gitOut(ctx, dir, args...); err != nil {
				return "", err
			}
		}
	}
	if _, err := gitOut(ctx, dir, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", err
	}
	return dir, nil
}

func (w *Workspaces) remote(repo string) string {
	if w.opts.Remote != nil {
		return w.opts.Remote(repo)
	}
	server := w.opts.Server
	if server == "" {
		server = "https://github.com"
	}
	return strings.TrimSuffix(server, "/") + "/" + repo + ".git"
}

// mainBranch returns the remote's default branch.
func mainBranch(ctx context.Context, clone string) (string, error) {
	out, err := gitOut(ctx, clone, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			name, _, _ := strings.Cut(rest, "\t")
			return name, nil
		}
	}
	return "", fmt.Errorf("the remote of %s names no default branch", clone)
}

// remoteBranch returns the ticket's branch on the remote, istari/<key>/*, or "".
func remoteBranch(ctx context.Context, clone string, ref tickets.Ref) (string, error) {
	prefix := "refs/remotes/origin/" + BranchPrefix + strconv.Itoa(ref.Number) + "/"
	out, err := gitOut(ctx, clone, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil || out == "" {
		return "", err
	}
	first, _, _ := strings.Cut(out, "\n")
	return strings.TrimPrefix(first, "refs/remotes/origin/"), nil
}

func hasRef(ctx context.Context, dir, ref string) bool {
	_, err := gitOut(ctx, dir, "rev-parse", "--verify", "-q", ref)
	return err == nil
}

func isWorktree(ctx context.Context, dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	_, err := gitOut(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

func repoName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	return name
}
