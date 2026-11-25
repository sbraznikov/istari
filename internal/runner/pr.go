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
	if based, main, err := p.cp.ws.BasedOnMain(ctx, ws, run.Dir); err != nil {
		return vcs.Opened{}, err
	} else if !based {
		return vcs.Opened{}, fmt.Errorf("pr: the branch is not based on the current %s: merge origin/%s, resolve, make a checkpoint, then open the pull request", main, main)
	}
	head, err := gitOut(ctx, run.Dir, "rev-parse", "HEAD")
	if err != nil {
		return vcs.Opened{}, err
	}
	// Only the result of a run by code goes into the journal (spec 7.7).
	entry := &journal.CheckpointEntry{Branch: ws.Branch, Commit: head[:min(12, len(head))], Checks: map[string]string{}}
	record := func(done string) error {
		entry.Done = done
		return p.cp.journal.Append(ctx, ref, journal.Entry{Kind: journal.Checkpoint, Actor: run.Agent(), Checkpoint: entry})
	}
	checks, err := p.cp.ws.checks(ctx, ws.Repo)
	if err != nil {
		return vcs.Opened{}, err
	}
	var red []string // the outputs of the red checks
	for _, check := range prChecks(checks) {
		if check.TakesTests() {
			entry.Notes = append(entry.Notes, check.Name+": not run, it runs the slice's tests through {files} or {dirs}; the whole suite is full")
			continue
		}
		if passed, output := runCheck(ctx, run.Dir, check.Run); passed {
			entry.Checks[check.Name] = "green"
		} else {
			entry.Checks[check.Name], entry.Red = "red", true
			if entry.Failures == nil {
				entry.Failures = map[string]string{}
			}
			entry.Failures[check.Name] = output
			red = append(red, check.Name+":\n"+output)
		}
	}
	if _, ok := entry.Checks["full"]; !ok {
		if _, ok := entry.Checks["test"]; !ok {
			entry.Notes = append(entry.Notes, "full: no command in the main branch's "+pipeline.Path)
		}
	}
	if len(red) > 0 {
		if err := record("the full checks fail: the pull request is not opened"); err != nil {
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
