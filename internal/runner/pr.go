package runner

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/sbraznikov/istari/internal/tickets"
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
	checks, _, err := p.cp.checks(ctx, ws.Repo)
	if err != nil {
		return vcs.Opened{}, err
	}
	full, ok := checks["full"]
	if !ok {
		full, ok = checks["test"] // without a full suite, the tests
	}
	if ok {
		if passed, output := runCheck(ctx, run.Dir, full); !passed {
			return vcs.Opened{}, fmt.Errorf("pr: the full checks fail, so the pull request is not opened:\n%s", output)
		}
	}
	url, private := p.cp.ws.PushTarget(ws.Repo)
	var token string
	if p.cp.tokens != nil {
		if token, err = p.cp.tokens.Token(ctx, ws.Repo); err != nil {
			return vcs.Opened{}, err
		}
	}
	if err := vcs.Push(ctx, vcs.PushSpec{Worktree: run.Dir, URL: url, Private: private, Own: ws.Branch, Target: ws.Branch, Force: true, Token: token}); err != nil {
		return vcs.Opened{}, err
	}
	clone := p.cp.ws.clonePath(ws.Repo)
	base, err := mainBranch(ctx, clone)
	if err != nil {
		return vcs.Opened{}, err
	}
	pr, err := p.client.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: ref.Repo, Ticket: run.Ticket, Head: ws.Branch, Base: base, Title: title, Body: body, Draft: true})
	if err != nil {
		return vcs.Opened{}, err
	}
	if err := p.watch.Track(ctx, run.Ticket, ref.Repo, pr.Number); err != nil {
		return pr, err
	}
	return pr, nil
}
