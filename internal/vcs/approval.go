package vcs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// codeownersPaths are where GitHub looks for CODEOWNERS, in its order.
var codeownersPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// Approval returns who approved the pull request's current head: a member
// listed in CODEOWNERS on the base branch, a team's member through the team
// (spec section 4.3). A reviewer's latest deciding review counts; a review of
// an older head does not, and a change request from anyone leaves the pull
// request unapproved. "" means no such approval.
func (c *Client) Approval(ctx context.Context, repo string, n int) (string, error) {
	var pr struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return "", err
	}
	latest, order, err := c.latestReviews(ctx, repo, n)
	if err != nil {
		return "", err
	}
	var approvers []string
	for _, who := range order {
		switch r := latest[who]; {
		case r.State == "CHANGES_REQUESTED":
			return "", nil
		case r.State == "APPROVED" && r.CommitID == pr.Head.SHA:
			approvers = append(approvers, who)
		}
	}
	if len(approvers) == 0 {
		return "", nil
	}
	owners, err := c.codeowners(ctx, repo, pr.Base.Ref)
	if err != nil {
		return "", err
	}
	for _, who := range approvers {
		owner, err := c.isOwner(ctx, repo, owners, who)
		if err != nil {
			return "", err
		}
		if owner {
			return who, nil
		}
	}
	return "", nil
}

// Author returns the login that opened the pull request, without a GitHub
// App the gh CLI's user (spec section 11), and the pull request's current
// head commit, which the author's istari gate approve binds to (spec 4.3).
func (c *Client) Author(ctx context.Context, repo string, n int) (login, head string, err error) {
	var pr struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return "", "", err
	}
	return pr.User.Login, pr.Head.SHA, nil
}

// review is a pull request review as Approval reads it.
type review struct {
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

// latestReviews returns each reviewer's latest deciding review (approved,
// changes requested or dismissed) and the reviewers in the order they first
// decided.
func (c *Client) latestReviews(ctx context.Context, repo string, n int) (map[string]review, []string, error) {
	latest := map[string]review{}
	var order []string
	for page := 1; ; page++ {
		var reviews []review
		if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100&page=%d", repo, n, page), nil, &reviews); err != nil {
			return nil, nil, err
		}
		for _, r := range reviews {
			if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" && r.State != "DISMISSED" {
				continue
			}
			if _, seen := latest[r.User.Login]; !seen {
				order = append(order, r.User.Login)
			}
			latest[r.User.Login] = r
		}
		if len(reviews) < 100 {
			return latest, order, nil
		}
	}
}

// codeowners returns the owners CODEOWNERS on ref lists: logins and
// org/team slugs, without the @. A repository without CODEOWNERS has none.
func (c *Client) codeowners(ctx context.Context, repo, ref string) ([]string, error) {
	for _, path := range codeownersPaths {
		body, _, err := c.sendAs(ctx, repo, http.MethodGet, fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", c.base, repo, path, url.QueryEscape(ref)), "application/vnd.github.raw+json", nil)
		if status(err) == http.StatusNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		return ownersIn(string(body)), nil
	}
	return nil, nil
}

// ownersIn reads the owners of every rule of a CODEOWNERS file; e-mail
// owners are not GitHub logins and are left out.
func ownersIn(codeowners string) []string {
	var owners []string
	for line := range strings.Lines(codeowners) {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		for _, f := range fields[min(1, len(fields)):] {
			if owner, ok := strings.CutPrefix(f, "@"); ok {
				owners = append(owners, owner)
			}
		}
	}
	return owners
}

// isOwner reports whether login is one of owners, directly or as an active
// member of an owning team.
func (c *Client) isOwner(ctx context.Context, repo string, owners []string, login string) (bool, error) {
	for _, owner := range owners {
		org, team, isTeam := strings.Cut(owner, "/")
		if !isTeam {
			if strings.EqualFold(owner, login) {
				return true, nil
			}
			continue
		}
		var m struct {
			State string `json:"state"`
		}
		err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/orgs/%s/teams/%s/memberships/%s", org, team, login), nil, &m)
		if status(err) == http.StatusNotFound {
			continue
		}
		if err != nil {
			return false, err
		}
		if m.State == "active" {
			return true, nil
		}
	}
	return false, nil
}
