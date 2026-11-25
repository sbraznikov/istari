package runner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

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

// fullRed is what the journal says of a pr open stopped by red full checks.
const fullRed = "the full checks fail: the pull request is not opened"

// lastCheckpoint returns the last checkpoint entry of a journal, or nil.
func lastCheckpoint(entries []journal.Entry) *journal.CheckpointEntry {
	for i := len(entries) - 1; i >= 0; i-- {
		if cp := entries[i].Checkpoint; cp != nil {
			return cp
		}
	}
	return nil
}

// PullRequests opens the pull requests of tickets (spec section 7.7: opening
// is gated by the full check suite run by code).
type PullRequests struct {
	cp     *Checkpoints
	client *vcs.Client
	watch  *vcs.Watcher
}

// NewPullRequests returns the opener.
func NewPullRequests(cp *Checkpoints, c *vcs.Client, w *vcs.Watcher) *PullRequests {
	return &PullRequests{cp: cp, client: c, watch: w}
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
	} else if !based {
		return vcs.Opened{}, fmt.Errorf("pr: the branch is not based on the current %s: merge origin/%s, resolve, make a checkpoint, then open the pull request", main, main)
	}
	head, err := gitOut(ctx, run.Dir, "rev-parse", "HEAD")
	if err != nil {
		return vcs.Opened{}, err
	}
	commit := head[:min(12, len(head))]
	entries, err := p.cp.journal.Stored(ctx, ref)
	if err != nil {
		return vcs.Opened{}, err
	}
	// The same code fails the same way: running the checks again only costs
	// time (spec 7.7, a red full suite).
	if last := lastCheckpoint(entries); last != nil && last.Done == fullRed && last.Commit == commit {
		return vcs.Opened{}, fmt.Errorf("pr: the full checks already failed on %s, and the branch has not changed since: change the code, or merge origin/%s, make a checkpoint, then open the pull request", commit, main)
	}
	// Only the result of a run by code goes into the journal (spec 7.7).
	entry := &journal.CheckpointEntry{Branch: ws.Branch, Commit: commit, Checks: map[string]string{}}
	record := func(done string) error {
		entry.Done = done
		return p.cp.journal.Append(ctx, ref, journal.Entry{Kind: journal.Checkpoint, Actor: run.Agent(), Checkpoint: entry})
	}
	checks, _, err := p.cp.ws.checks(ctx, ws.Repo)
	if err != nil {
		return vcs.Opened{}, err
	}
	name := "full"
	full, ok := checks[name]
	if !ok {
		name = "test" // without a full suite, the tests
		full, ok = checks[name]
	}
	if !ok {
		entry.Notes = append(entry.Notes, "full: no command in the main branch's "+pipeline.Path)
	} else if passed, output := runCheck(ctx, run.Dir, full); passed {
		entry.Checks[name] = "green"
	} else {
		entry.Checks[name], entry.Red, entry.Failures = "red", true, map[string]string{name: output}
		if err := record(fullRed); err != nil {
			return vcs.Opened{}, err
		}
		return vcs.Opened{}, fmt.Errorf("pr: the full checks fail, so the pull request is not opened:\n%s", output)
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
