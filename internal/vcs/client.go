package vcs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/github/ratelimit"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Client talks to GitHub for pull requests, checks and reviews.
type Client struct {
	base   string
	tokens Tokens
	http   *http.Client
}

// NewClient returns a client with its own ratelimit.Transport; baseURL ""
// means api.github.com.
func NewClient(baseURL string, tokens Tokens) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &Client{base: strings.TrimSuffix(baseURL, "/"), tokens: tokens, http: &http.Client{Timeout: 30 * time.Second, Transport: ratelimit.New(ratelimit.Options{})}}
}

// WithTransport returns a copy of the client that sends its requests through
// rt: the daemon shares one ratelimit.Transport among its GitHub clients
// (#208).
func (c *Client) WithTransport(rt http.RoundTripper) *Client {
	out := *c
	out.http = &http.Client{Timeout: c.http.Timeout, Transport: rt}
	return &out
}

// Tokens returns the client's token source.
func (c *Client) Tokens() Tokens { return c.tokens }

// NewPullRequest is a pull request to open for a ticket.
type NewPullRequest struct {
	Repo, Ticket, Head, Base, Title, Body string
	Draft                                 bool // opened as a draft, until review
}

// Opened is an opened pull request.
type Opened struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
}

// PullRequestAt reads a pull request's number from its URL
// (…/pull/<number>), as the journal records it.
func PullRequestAt(url string) (Opened, bool) {
	i := strings.LastIndex(url, "/pull/")
	if i < 0 {
		return Opened{}, false
	}
	n, err := strconv.Atoi(url[i+len("/pull/"):])
	if err != nil || n <= 0 {
		return Opened{}, false
	}
	return Opened{Number: n, URL: url}, true
}

// OpenPullRequest opens a pull request that links to its ticket: the body
// ends with "Closes #<n>" for an issue of the same repository and names the
// ticket (spec section 7.7: the link to the ticket is set by code).
func (c *Client) OpenPullRequest(ctx context.Context, p NewPullRequest) (Opened, error) {
	ref, err := tickets.ParseRef(p.Ticket)
	if err != nil {
		return Opened{}, err
	}
	var pr Opened
	err = c.do(ctx, p.Repo, http.MethodPost, "/repos/"+p.Repo+"/pulls", map[string]any{"title": p.Title, "body": linked(p, ref), "head": p.Head, "base": p.Base, "draft": p.Draft}, &pr)
	return pr, err
}

// UpdatePullRequest sets a pull request's title and body; the link to its
// ticket is kept.
func (c *Client) UpdatePullRequest(ctx context.Context, p NewPullRequest, n int) error {
	ref, err := tickets.ParseRef(p.Ticket)
	if err != nil {
		return err
	}
	return c.do(ctx, p.Repo, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", p.Repo, n), map[string]string{"title": p.Title, "body": linked(p, ref)}, nil)
}

// linked is a pull request's body with its ticket link.
func linked(p NewPullRequest, ref tickets.Ref) string {
	body := strings.TrimSpace(p.Body)
	if ref.Provider == "gh" && strings.EqualFold(ref.Repo, p.Repo) {
		body += fmt.Sprintf("\n\nCloses #%d", ref.Number)
	}
	return strings.TrimSpace(body + "\n\nTicket: " + p.Ticket)
}

// Status is what a pull request's checks and reviews say.
type Status struct {
	HeadSHA string
	Checks  string // "", pending, success or failure
	Review  string // none, approved or changes_requested
	By      string // who gave the deciding review
	Merged  bool
	Closed  bool
}

// ReviewNone is the review state of a pull request without a deciding review.
const ReviewNone = "none"

// checks sums up a commit's check runs and its combined status: failure if
// any failed, pending if any still runs, success if all passed, "" if there
// are none.
func (c *Client) checks(ctx context.Context, repo, sha string) (string, error) {
	state := ""
	add := func(s string) {
		switch {
		case s == "failure" || state == "failure":
			state = "failure"
		case s == "pending" || state == "pending":
			state = "pending"
		case s == "success":
			state = "success"
		}
	}
	for page := 1; ; page++ {
		var runs struct {
			CheckRuns []struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100&page=%d", repo, sha, page), nil, &runs); err != nil {
			return "", err
		}
		for _, r := range runs.CheckRuns {
			switch {
			case r.Status != "completed":
				add("pending")
			case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
				add("success")
			default: // failure, cancelled, timed_out, action_required, stale
				add("failure")
			}
		}
		if len(runs.CheckRuns) < 100 {
			break
		}
	}
	var combined struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/status", repo, sha), nil, &combined); err != nil {
		return "", err
	}
	if combined.TotalCount > 0 {
		if combined.State == "error" {
			combined.State = "failure"
		}
		add(combined.State)
	}
	return state, nil
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
		checks, err := c.checks(ctx, repo, s.HeadSHA)
		if err != nil {
			return Status{}, err
		}
		s.Checks = checks
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
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" && r.State != "DISMISSED" {
			continue
		}
		if _, seen := latest[r.User.Login]; !seen {
			order = append(order, r.User.Login)
		}
		latest[r.User.Login] = r.State
	}
	s.Review = ReviewNone
	for _, who := range order {
		switch latest[who] {
		case "CHANGES_REQUESTED":
			s.Review, s.By = "changes_requested", who
		case "APPROVED":
			if s.Review == ReviewNone {
				s.Review, s.By = "approved", who
			}
		}
	}
	return s, nil
}

// Axis is one axis of a review: its summary line and its findings.
type Axis struct {
	Summary  string
	Findings []string
}

// Review is the report istari publishes on a pull request (spec 7.6): the
// axes Standards and Spec apart, neither ranked against the other.
type Review struct {
	Standards, Spec Axis
}

// PublishReview posts the review as a comment review: the verdict is
// istari's, so the review neither approves nor requests changes.
func (c *Client) PublishReview(ctx context.Context, repo string, n int, r Review) error {
	var b strings.Builder
	for _, a := range []struct {
		name string
		axis Axis
	}{{"Standards", r.Standards}, {"Spec", r.Spec}} {
		fmt.Fprintf(&b, "## %s\n\n%s\n", a.name, a.axis.Summary)
		for _, f := range a.axis.Findings {
			b.WriteString("\n- " + f)
		}
		b.WriteString("\n\n")
	}
	body := map[string]string{"body": strings.TrimSpace(b.String()), "event": "COMMENT"}
	return c.do(ctx, repo, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, n), body, nil)
}

// Merge merges the pull request with the method (squash, merge or rebase)
// and deletes its branch.
func (c *Client) Merge(ctx context.Context, repo string, n int, method string) error {
	var pr struct {
		Head struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return err
	}
	if err := c.do(ctx, repo, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, n), map[string]string{"merge_method": method}, nil); err != nil {
		return err
	}
	if !strings.EqualFold(pr.Head.Repo.FullName, repo) {
		return nil // a fork's branch is not ours to delete
	}
	err := c.do(ctx, repo, http.MethodDelete, "/repos/"+repo+"/git/refs/heads/"+pr.Head.Ref, nil, nil)
	if err != nil && strings.Contains(err.Error(), ": 422 ") {
		return nil // deleted already, e.g. by the repository's auto-delete
	}
	return err
}

// MarkReady marks a draft pull request ready for review; a pull request
// that is no draft is left as it is. GitHub's REST API cannot end a draft,
// so this goes through its GraphQL API.
func (c *Client) MarkReady(ctx context.Context, repo string, n int) error {
	var pr struct {
		NodeID string `json:"node_id"`
		Draft  bool   `json:"draft"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return err
	}
	if !pr.Draft {
		return nil
	}
	var res struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	query := `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`
	if err := c.request(ctx, repo, http.MethodPost, c.graphqlURL(), map[string]any{"query": query, "variables": map[string]string{"id": pr.NodeID}}, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("github: mark pull request %d ready: %s", n, res.Errors[0].Message)
	}
	return nil
}

// graphqlURL is the GraphQL endpoint next to the REST base: /graphql on
// api.github.com, /api/graphql on GitHub Enterprise (REST at /api/v3).
func (c *Client) graphqlURL() string {
	if b, ok := strings.CutSuffix(c.base, "/v3"); ok {
		return b + "/graphql"
	}
	return c.base + "/graphql"
}

func (c *Client) do(ctx context.Context, repo, method, path string, body, out any) error {
	return c.request(ctx, repo, method, c.base+path, body, out)
}

func (c *Client) request(ctx context.Context, repo, method, url string, body, out any) error {
	token, err := c.tokens.Token(ctx, repo)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
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
		if ratelimit.RateLimited(res) {
			return fmt.Errorf("github: %s %s: %d %s: %w", method, strings.TrimPrefix(url, c.base), res.StatusCode, e.Message, ratelimit.ErrRateLimited)
		}
		return fmt.Errorf("github: %s %s: %d %s", method, strings.TrimPrefix(url, c.base), res.StatusCode, e.Message)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// CodePushRefused refuses a push outside the agent's own ticket branch.
const CodePushRefused = "push_refused"

// PushSpec is one push of an agent's work.
type PushSpec struct {
	Worktree string // the agent's worktree: where the commits are
	URL      string // the repository's own URL, as istari knows it
	Private  string // istari's private bare repository the push is made from
	Own      string // the ticket's branch
	Target   string // the branch asked for
	Force    bool   // with a lease on what istari last pushed or saw
	Token    string
}

// Push pushes the worktree's HEAD to the ticket's own branch, for an agent
// (spec section 2.1: only branches of its ticket). Any other target is
// refused. The worktree shares its config with the agent, so the push is not
// made from it: istari fetches the commits into a private repository of its
// own and pushes from there to the repository's URL, with no hooks and the
// token in git's environment, not its arguments. A forced push leases on what
// istari itself last pushed or saw, so work pushed by someone else is never
// overwritten.
func Push(ctx context.Context, p PushSpec) error {
	refuse := func(format string, args ...any) error {
		return &core.Error{Code: CodePushRefused, Message: fmt.Sprintf(format, args...)}
	}
	if p.Target != p.Own {
		return refuse("an agent pushes only to its ticket branch %s, not %s", p.Own, p.Target)
	}
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Dir = p.Private
		cmd.Env = append(cleanEnv(), env...)
		out, err := cmd.CombinedOutput()
		msg := strings.TrimSpace(string(out))
		if p.Token != "" {
			msg = strings.ReplaceAll(msg, p.Token, "***")
		}
		if err != nil {
			return msg, refuse("git %s: %s", args[0], msg)
		}
		return msg, nil
	}
	if _, err := os.Stat(p.Private); err != nil {
		if err := os.MkdirAll(p.Private, 0o700); err != nil {
			return err
		}
		if _, err := run(nil, "init", "-q", "--bare"); err != nil {
			return err
		}
	}
	work := "refs/istari/push/" + p.Own
	seen := "refs/istari/seen/" + p.Own
	if _, err := run(nil, "fetch", "-q", "--no-tags", p.Worktree, "+HEAD:"+work); err != nil {
		return err
	}
	auth := authEnv(p.Token)
	args := []string{"push", "--porcelain"}
	if p.Force {
		expected, err := run(nil, "rev-parse", "--verify", "-q", seen)
		if err != nil {
			// Nothing pushed from here yet: lease on the branch as it is now.
			expected = ""
			if out, err := run(auth, "ls-remote", p.URL, "refs/heads/"+p.Own); err == nil && out != "" {
				expected, _, _ = strings.Cut(out, "\t")
			}
		}
		args = append(args, "--force-with-lease=refs/heads/"+p.Own+":"+expected)
	}
	args = append(args, p.URL, work+":refs/heads/"+p.Own)
	if _, err := run(auth, args...); err != nil {
		return err
	}
	_, err := run(nil, "update-ref", seen, work)
	return err
}

// authEnv gives git the token as a header through its environment.
func authEnv(token string) []string {
	if token == "" {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + basic}
}

// cleanEnv is the daemon's environment without git's own variables or any
// user or system git config.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}
