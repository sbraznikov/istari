package runner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
)

var (
	sectionRe = regexp.MustCompile(`(?m)^##\s+(.+?)\s*$`)
	doorRe    = regexp.MustCompile(`(?i)\b(one|two)-way\b`)
)

// CheckDescription checks a pull request's description (spec section 7.6,
// after pr): the sections Summary, Evidence and Merge Danger, and a Merge
// Danger that says whether the change is a one-way or a two-way door.
func CheckDescription(body string) error {
	sections := map[string]string{}
	locs := sectionRe.FindAllStringSubmatchIndex(body, -1)
	for i, l := range locs {
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		sections[strings.ToLower(body[l[2]:l[3]])] = body[l[1]:end]
	}
	for _, name := range []string{"Summary", "Evidence", "Merge Danger"} {
		if strings.TrimSpace(sections[strings.ToLower(name)]) == "" {
			return fmt.Errorf("pr: the description needs a \"## %s\" section with content", name)
		}
	}
	if !doorRe.MatchString(sections["merge danger"]) {
		return fmt.Errorf("pr: Merge Danger must say whether the change is a one-way or a two-way door")
	}
	return nil
}

// prChecks are the checks that run before istari pr open: every check at pr,
// and the tests when there is no full suite.
func prChecks(checks pipeline.Checks) []pipeline.Check {
	var out []pipeline.Check
	_, full := checks.Get("full")
	for _, c := range checks.List {
		if c.RunsAt(pipeline.AtPR) || (c.Name == "test" && !full) {
			out = append(out, c)
		}
	}
	return out
}

// ErrMainRed is the error of a pr open whose full checks fail on the main
// branch too (spec 7.7): the ticket is held needs_input, and the run's task is
// released for the run after the hold.
var ErrMainRed = errors.New("a human is asked and the ticket is held needs_input")

// fullRed is what the journal says of a pr open stopped by red full checks.
const fullRed = "the full checks fail: the pull request is not opened"

// lastCheckpoint returns the last checkpoint entry of a journal that match
// accepts, or nil.
func lastCheckpoint(entries []journal.Entry, match func(*journal.CheckpointEntry) bool) *journal.CheckpointEntry {
	for i := len(entries) - 1; i >= 0; i-- {
		if cp := entries[i].Checkpoint; cp != nil && match(cp) {
			return cp
		}
	}
	return nil
}

// mainRed tells whether a checkpoint entry found the main branch red too.
func mainRed(cp *journal.CheckpointEntry) bool { return cp.Main != nil && cp.Main.Result == "red" }

// anyCheckpoint accepts every checkpoint entry.
func anyCheckpoint(*journal.CheckpointEntry) bool { return true }

// shortCommit is the form of a commit the journal keeps: its first 12
// characters, so that entries compare by the same string.
func shortCommit(sha string) string { return sha[:min(12, len(sha))] }

// PullRequests opens the pull requests of tickets (spec section 7.7: opening
// is gated by the full check suite run by code).
type PullRequests struct {
	cp      *Checkpoints
	client  *vcs.Client
	watch   *vcs.Watcher
	tickets *core.Tickets // holds a ticket whose main branch fails its checks too
}

// NewPullRequests returns the opener.
func NewPullRequests(cp *Checkpoints, c *vcs.Client, w *vcs.Watcher, t *core.Tickets) *PullRequests {
	return &PullRequests{cp: cp, client: c, watch: w, tickets: t}
}

// Open runs the full checks of the main branch's pipeline in the run's
// worktree, checks the description, pushes the branch and opens a draft pull
// request linked to the ticket, which istari then watches for CI and reviews.
func (p *PullRequests) Open(ctx context.Context, run Run, title, body string) (vcs.Opened, error) {
	if err := CheckDescription(body); err != nil {
		return vcs.Opened{}, err
	}
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return vcs.Opened{}, err
	}
	ws, err := p.cp.ws.Get(ctx, run.Ticket)
	if err != nil {
		return vcs.Opened{}, err
	}
	if ws.Branch == "" || !isWorktree(ctx, run.Dir) {
		return vcs.Opened{}, fmt.Errorf("pr: %s has no worktree", run.Ticket)
	}
	if err := OnBranch(ctx, ws, run.Dir); err != nil {
		return vcs.Opened{}, fmt.Errorf("pr: %w", err)
	}
	// The checks must judge the code the pull request gets.
	if dirty, err := gitOut(ctx, run.Dir, "status", "--porcelain"); err != nil {
		return vcs.Opened{}, err
	} else if dirty != "" {
		return vcs.Opened{}, fmt.Errorf("pr: the worktree has uncommitted changes; commit them with istari checkpoint first:\n%s", dirty)
	}
	based, main, err := p.cp.ws.BasedOnMain(ctx, ws, run.Dir)
	if err != nil {
		return vcs.Opened{}, err
	}
	if !based {
		return vcs.Opened{}, fmt.Errorf("pr: the branch is not based on the current %s: merge origin/%s, resolve, make a checkpoint, then open the pull request", main, main)
	}
	head, err := gitOut(ctx, run.Dir, "rev-parse", "HEAD")
	if err != nil {
		return vcs.Opened{}, err
	}
	commit := shortCommit(head)
	entries, err := p.cp.journal.Stored(ctx, ref)
	if err != nil {
		return vcs.Opened{}, err
	}
	// The same code fails the same way: running the checks again only costs
	// time (spec 7.7, a red full suite). A red that main failed too was not
	// the code's: after the hold is lifted, the checks run again.
	if last := lastCheckpoint(entries, anyCheckpoint); last != nil && last.Done == fullRed && last.Commit == commit && !mainRed(last) {
		return vcs.Opened{}, fmt.Errorf("pr: the full checks already failed on %s, and the branch has not changed since: change the code, or merge origin/%s, make a checkpoint, then open the pull request", commit, main)
	}
	// Only the result of a run by code goes into the journal (spec 7.7).
	entry := &journal.CheckpointEntry{Branch: ws.Branch, Commit: commit, Checks: map[string]string{}}
	record := func(done string) error {
		entry.Done = done
		return p.cp.journal.Append(ctx, ref, journal.Entry{Kind: journal.Checkpoint, Actor: run.Agent(), Checkpoint: entry})
	}
	checks, err := p.cp.ws.checks(ctx, ws.Repo)
	if err != nil {
		return vcs.Opened{}, err
	}
	suite := "full" // the whole suite; without one, the tests
	if _, ok := checks.Get(suite); !ok {
		suite = "test"
	}
	var red []string // the outputs of the red checks
	for _, check := range prChecks(checks) {
		if check.TakesTests() {
			entry.Notes = append(entry.Notes, check.Name+": not run, it runs the slice's tests through {files} or {dirs}; the whole suite is full")
			continue
		}
		passed, output := recordCheck(ctx, entry, run.Dir, check.Name, check.Run)
		if passed {
			continue
		}
		red = append(red, check.Name+":\n"+output)
		// Whether a red suite is the ticket's failure, the main branch tells.
		if check.Name == suite {
			if entry.Main, err = p.onMain(ctx, run.Ticket, ws, main, check.Name, check.Run, entries); err != nil {
				return vcs.Opened{}, err
			}
		}
	}
	if _, ok := entry.Checks[suite]; !ok {
		entry.Notes = append(entry.Notes, "full: no command in the main branch's "+pipeline.Path)
	}
	if len(red) > 0 {
		if mainRed(entry) {
			question := fmt.Sprintf("%s fails `%s` at %s", main, entry.Main.Check, entry.Main.Commit)
			entry.Notes = append(entry.Notes, question+" as well: the failure is not the ticket's")
			if err := record(fullRed); err != nil {
				return vcs.Opened{}, err
			}
			if err := p.ask(ctx, run, ref, question); err != nil {
				return vcs.Opened{}, err
			}
			return vcs.Opened{}, fmt.Errorf("pr: the full checks fail, and %s; %w: end your run, do not try again:\n%s", question, ErrMainRed, strings.Join(red, "\n\n"))
		}
		if err := record(fullRed); err != nil {
			return vcs.Opened{}, err
		}
		return vcs.Opened{}, fmt.Errorf("pr: the full checks fail, so the pull request is not opened:\n%s", strings.Join(red, "\n\n"))
	}
	if err := p.cp.push(ctx, ws, run.Dir); err != nil {
		return vcs.Opened{}, err
	}
	clone := p.cp.ws.clonePath(ws.Repo)
	base, err := mainBranch(ctx, clone)
	if err != nil {
		return vcs.Opened{}, err
	}
	// One pull request per ticket: a later round updates it (spec 4.3).
	np := vcs.NewPullRequest{Repo: ref.Repo, Ticket: run.Ticket, Head: ws.Branch, Base: base, Title: title, Body: body, Draft: true}
	done := "opened"
	var pr vcs.Opened
	if t, err := p.watch.PullRequestOf(ctx, run.Ticket); err == nil && !t.Done {
		if err := p.client.UpdatePullRequest(ctx, np, t.Number); err != nil {
			return vcs.Opened{}, err
		}
		pr, done = vcs.Opened{Number: t.Number, URL: t.URL}, "updated"
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return vcs.Opened{}, err
	} else {
		if pr, err = p.client.OpenPullRequest(ctx, np); err != nil {
			return vcs.Opened{}, err
		}
		if err := p.watch.Track(ctx, run.Ticket, ref.Repo, pr); err != nil {
			return pr, err
		}
	}
	if err := p.watch.Pushed(ctx, run.Ticket, head); err != nil {
		return pr, err
	}
	entry.PR = pr.URL
	return pr, record(fmt.Sprintf("pull request #%d %s", pr.Number, done))
}

// ask raises a question for a human in the ticket's journal and holds the
// ticket needs_input until it is answered (spec 4.1).
func (p *PullRequests) ask(ctx context.Context, run Run, ref tickets.Ref, question string) error {
	q := journal.Entry{Kind: journal.Question, Actor: "system", ID: run.ID + "-main", Text: question}
	if err := p.cp.journal.Append(ctx, ref, q); err != nil {
		return err
	}
	_, err := p.tickets.SetHold(ctx, "system", run.Ticket, "needs_input")
	var ce *core.Error
	if errors.As(err, &ce) && ce.Code == core.CodeTicketHeld {
		return nil
	}
	return err
}

// mainSuffix marks the scratch worktree, next to the ticket's own, in which
// a red check runs again on the main branch's tip.
const mainSuffix = ".main"

// onMain returns the result of a red check on the main branch's tip, which
// tells whether the failure is the ticket's (spec 7.7). It is kept per main
// commit: a result the journal has for the tip is used again; otherwise the
// command runs in a scratch checkout of the tip, and a red run once more.
func (p *PullRequests) onMain(ctx context.Context, ticket string, ws Workspace, main, name, cmd string, entries []journal.Entry) (*journal.MainCheck, error) {
	tip, err := gitOut(ctx, p.cp.ws.clonePath(ws.Repo), "rev-parse", "refs/remotes/origin/"+main)
	if err != nil {
		return nil, err
	}
	onTip := func(cp *journal.CheckpointEntry) bool {
		return cp.Main != nil && cp.Main.Commit == shortCommit(tip) && cp.Main.Check == name
	}
	if cp := lastCheckpoint(entries, onTip); cp != nil {
		return cp.Main, nil
	}
	dir := ws.Dir + mainSuffix
	remove, err := p.cp.ws.Scratch(ctx, ticket, dir)
	if err != nil {
		return nil, err
	}
	defer remove()
	// Scratch fetched again: the result is of the commit it checked out.
	if tip, err = gitOut(ctx, dir, "rev-parse", "HEAD"); err != nil {
		return nil, err
	}
	m := &journal.MainCheck{Commit: shortCommit(tip), Check: name, Result: "red"}
	// One red run can be a flaky test: only a second red is kept.
	for range 2 {
		passed, output := runCheck(ctx, dir, cmd)
		if passed {
			m.Result, m.Output = "green", ""
			break
		}
		m.Output = output
	}
	return m, nil
}
