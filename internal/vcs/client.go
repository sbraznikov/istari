package vcs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Client talks to GitHub for pull requests, checks and reviews.
type Client struct {
	base   string
	tokens Tokens
	http   *http.Client
}

// NewClient returns a client; baseURL "" means api.github.com.
func NewClient(baseURL string, tokens Tokens) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &Client{base: strings.TrimSuffix(baseURL, "/"), tokens: tokens, http: &http.Client{Timeout: 30 * time.Second}}
}

// Tokens returns the client's token source.
func (c *Client) Tokens() Tokens { return c.tokens }

// NewPullRequest is a pull request to open for a ticket.
type NewPullRequest struct {
	Repo, Ticket, Head, Base, Title, Body string
}

// Opened is an opened pull request.
type Opened struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
}

// OpenPullRequest opens a pull request that links to its ticket: the body
// ends with "Closes #<n>" for an issue of the same repository and names the
// ticket (spec section 7.7: the link to the ticket is set by code).
func (c *Client) OpenPullRequest(ctx context.Context, p NewPullRequest) (Opened, error) {
	ref, err := tickets.ParseRef(p.Ticket)
	if err != nil {
		return Opened{}, err
	}
	body := strings.TrimSpace(p.Body)
	if ref.Provider == "gh" && strings.EqualFold(ref.Repo, p.Repo) {
		body += fmt.Sprintf("\n\nCloses #%d", ref.Number)
	}
	body = strings.TrimSpace(body + "\n\nTicket: " + p.Ticket)
	var pr Opened
	err = c.do(ctx, p.Repo, http.MethodPost, "/repos/"+p.Repo+"/pulls", map[string]string{"title": p.Title, "body": body, "head": p.Head, "base": p.Base}, &pr)
	return pr, err
}

// Status is what a pull request's checks and reviews say.
type Status struct {
	HeadSHA string
	Checks  string // "", pending, success or failure
	Review  string // "", approved or changes_requested
	By      string // who gave the deciding review
	Merged  bool
	Closed  bool
}

// Status reads the pull request's head, its checks and its reviews.
func (c *Client) Status(ctx context.Context, repo string, n int) (Status, error) {
	var pr struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return Status{}, err
	}
	s := Status{HeadSHA: pr.Head.SHA, Merged: pr.Merged, Closed: pr.State == "closed"}
	if s.HeadSHA != "" {
		var runs struct {
			CheckRuns []struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := c.do(ctx, repo, http.MethodGet, "/repos/"+repo+"/commits/"+s.HeadSHA+"/check-runs?per_page=100", nil, &runs); err != nil {
			return Status{}, err
		}
		for _, r := range runs.CheckRuns {
			switch {
			case r.Status != "completed":
				if s.Checks != "failure" {
					s.Checks = "pending"
				}
			case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
				if s.Checks == "" {
					s.Checks = "success"
				}
			default:
				s.Checks = "failure"
			}
		}
	}
	var reviews []struct {
		State string `json:"state"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repo, n), nil, &reviews); err != nil {
		return Status{}, err
	}
	latest := map[string]string{} // a reviewer's latest deciding review counts
	var order []string
	for _, r := range reviews {
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" {
			continue
		}
		if _, seen := latest[r.User.Login]; !seen {
			order = append(order, r.User.Login)
		}
		latest[r.User.Login] = r.State
	}
	for _, who := range order {
		switch latest[who] {
		case "CHANGES_REQUESTED":
			s.Review, s.By = "changes_requested", who
		case "APPROVED":
			if s.Review == "" {
				s.Review, s.By = "approved", who
			}
		}
	}
	return s, nil
}

// Merge merges the pull request with the method (squash, merge or rebase)
// and deletes its branch.
func (c *Client) Merge(ctx context.Context, repo string, n int, method string) error {
	var pr struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return err
	}
	if err := c.do(ctx, repo, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, n), map[string]string{"merge_method": method}, nil); err != nil {
		return err
	}
	return c.do(ctx, repo, http.MethodDelete, "/repos/"+repo+"/git/refs/heads/"+pr.Head.Ref, nil, nil)
}

func (c *Client) do(ctx context.Context, repo, method, path string, body, out any) error {
	token, err := c.tokens.Token(ctx, repo)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		var e struct{ Message string }
		json.Unmarshal(data, &e)
		return fmt.Errorf("github: %s %s: %d %s", method, path, res.StatusCode, e.Message)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// CodePushRefused refuses a push outside the agent's own ticket branch.
const CodePushRefused = "push_refused"

// Push pushes the worktree's HEAD to the ticket's own branch on origin, for
// an agent (spec section 2.1: the token may push only to its ticket branch).
// Any other target is refused. force pushes with a lease on what istari last
// saw of the branch, so work pushed by someone else is never overwritten.
// The token, if any, goes to git as a header of this one command.
func Push(ctx context.Context, dir, own, target string, force bool, token string) error {
	if target != own {
		return &core.Error{Code: CodePushRefused, Message: fmt.Sprintf("an agent pushes only to its ticket branch %s, not %s", own, target)}
	}
	var args []string
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		args = append(args, "-c", "http.extraheader=AUTHORIZATION: basic "+basic)
	}
	args = append(args, "push", "--porcelain")
	if force {
		args = append(args, "--force-with-lease=refs/heads/"+own)
	}
	args = append(args, "origin", "HEAD:refs/heads/"+own)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "***")
		}
		return &core.Error{Code: CodePushRefused, Message: "git push: " + msg}
	}
	return nil
}
