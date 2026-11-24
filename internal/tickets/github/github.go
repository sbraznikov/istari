// Package github is the GitHub Issues adapter of tickets.Provider (spec
// section 5). The ticket state is exactly one label istari:<state>, a hold is
// a label istari:hold:<reason>; done closes the issue as completed and
// canceled as not planned (spec section 4.1).
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// DefaultBaseURL is the GitHub REST API.
const DefaultBaseURL = "https://api.github.com"

const (
	statePrefix = "istari:"
	holdPrefix  = "istari:hold:"
)

// Options configures the adapter.
type Options struct {
	BaseURL    string // default DefaultBaseURL
	Token      string // a token that may read and write issues and labels
	HTTPClient *http.Client
}

// Provider talks to GitHub Issues.
type Provider struct {
	base   string
	token  string
	http   *http.Client
	mu     sync.Mutex
	labels map[string]bool // "owner/repo\x00label" known to exist
}

var _ tickets.Provider = (*Provider)(nil)

// New returns the adapter.
func New(opts Options) *Provider {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	return &Provider{base: strings.TrimRight(opts.BaseURL, "/"), token: opts.Token, http: opts.HTTPClient, labels: map[string]bool{}}
}

// Capabilities implements tickets.Provider.
func (p *Provider) Capabilities() tickets.Caps {
	return tickets.Caps{SubIssues: true, CustomStates: false, Webhooks: true}
}

// Subscribe implements tickets.Provider. Webhooks and polling come with the
// sync loop (#16).
func (p *Provider) Subscribe(context.Context) (<-chan tickets.Event, error) {
	return nil, tickets.ErrNotImplemented
}

type issue struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	HTMLURL     string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (i issue) ticket(repo string) tickets.Ticket {
	t := tickets.Ticket{
		Ref:   tickets.Ref{Provider: "gh", Repo: repo, Number: i.Number},
		Title: i.Title, Body: i.Body, Author: i.User.Login, Closed: i.State == "closed", URL: i.HTMLURL,
		Labels: make([]string, 0, len(i.Labels)),
	}
	for _, l := range i.Labels {
		t.Labels = append(t.Labels, l.Name)
	}
	t.State = stateOf(t.Labels)
	return t
}

// stateOf reads istari's state and hold from labels. With several state
// labels (a human edit) the first one counts until istari rewrites them.
func stateOf(labels []string) tickets.State {
	var s tickets.State
	for _, l := range labels {
		if hold, ok := strings.CutPrefix(l, holdPrefix); ok {
			if s.Hold == "" {
				s.Hold = hold
			}
		} else if state, ok := strings.CutPrefix(l, statePrefix); ok && s.State == "" {
			s.State = core.TicketState(state)
		}
	}
	return s
}

func repoOf(ref tickets.Ref) (string, error) {
	if ref.Provider != "gh" || ref.Repo == "" || ref.Number < 1 {
		return "", fmt.Errorf("github: %s is not a GitHub issue reference", ref)
	}
	return ref.Repo, nil
}

// Get implements tickets.Provider.
func (p *Provider) Get(ctx context.Context, ref tickets.Ref) (tickets.Ticket, error) {
	repo, err := repoOf(ref)
	if err != nil {
		return tickets.Ticket{}, err
	}
	var i issue
	if err := p.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, ref.Number), nil, &i); err != nil {
		return tickets.Ticket{}, fmt.Errorf("github: get %s: %w", ref, err)
	}
	if i.PullRequest != nil {
		return tickets.Ticket{}, fmt.Errorf("github: %s is a pull request: %w", ref, tickets.ErrNotFound)
	}
	return i.ticket(repo), nil
}

// List implements tickets.Provider. Pull requests, which the issues API also
// returns, are left out.
func (p *Provider) List(ctx context.Context, q tickets.Query) ([]tickets.Ticket, error) {
	v := url.Values{"per_page": {"100"}, "state": {"open"}}
	if q.Closed {
		v.Set("state", "all")
	}
	if len(q.Labels) > 0 {
		v.Set("labels", strings.Join(q.Labels, ","))
	}
	path := fmt.Sprintf("/repos/%s/issues?%s", q.Repo, v.Encode())
	var out []tickets.Ticket
	for path != "" {
		var page []issue
		next, err := p.doPage(ctx, path, &page)
		if err != nil {
			return nil, fmt.Errorf("github: list %s: %w", q.Repo, err)
		}
		for _, i := range page {
			if i.PullRequest == nil {
				out = append(out, i.ticket(q.Repo))
			}
		}
		path = next
	}
	return out, nil
}

// Create implements tickets.Provider: the issue is created in state new (T1),
// and labels missing from the repository are created first.
func (p *Provider) Create(ctx context.Context, d tickets.Draft) (tickets.Ticket, error) {
	labels := append(slices.Clone(d.Labels), statePrefix+string(core.TicketNew))
	if err := p.ensureLabels(ctx, d.Repo, labels); err != nil {
		return tickets.Ticket{}, err
	}
	var i issue
	req := map[string]any{"title": d.Title, "body": d.Body, "labels": labels}
	if err := p.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues", d.Repo), req, &i); err != nil {
		return tickets.Ticket{}, fmt.Errorf("github: create in %s: %w", d.Repo, err)
	}
	return i.ticket(d.Repo), nil
}

// SetState implements tickets.Provider. It replaces istari's labels with
// exactly one state label and at most one hold label, keeps every other
// label, and closes the issue for done (completed) and canceled (not
// planned) or reopens it for any other state.
func (p *Provider) SetState(ctx context.Context, ref tickets.Ref, s tickets.State) error {
	current, err := p.Get(ctx, ref)
	if err != nil {
		return err
	}
	labels := slices.DeleteFunc(slices.Clone(current.Labels), func(l string) bool { return strings.HasPrefix(l, statePrefix) })
	labels = append(labels, statePrefix+string(s.State))
	if s.Hold != "" {
		labels = append(labels, holdPrefix+s.Hold)
	}
	if err := p.ensureLabels(ctx, ref.Repo, labels); err != nil {
		return err
	}
	if !slices.Equal(labels, current.Labels) {
		if err := p.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/issues/%d/labels", ref.Repo, ref.Number), map[string]any{"labels": labels}, nil); err != nil {
			return fmt.Errorf("github: set labels of %s: %w", ref, err)
		}
	}
	var patch map[string]any
	switch {
	case s.State == core.TicketDone:
		patch = map[string]any{"state": "closed", "state_reason": "completed"}
	case s.State == core.TicketCanceled:
		patch = map[string]any{"state": "closed", "state_reason": "not_planned"}
	case current.Closed:
		patch = map[string]any{"state": "open", "state_reason": "reopened"}
	}
	if patch != nil {
		if err := p.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", ref.Repo, ref.Number), patch, nil); err != nil {
			return fmt.Errorf("github: set state of %s: %w", ref, err)
		}
	}
	return nil
}

// Comment implements tickets.Provider.
func (p *Provider) Comment(ctx context.Context, ref tickets.Ref, body string) (tickets.CommentRef, error) {
	repo, err := repoOf(ref)
	if err != nil {
		return tickets.CommentRef{}, err
	}
	var c struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := p.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, ref.Number), map[string]any{"body": body}, &c); err != nil {
		return tickets.CommentRef{}, fmt.Errorf("github: comment on %s: %w", ref, err)
	}
	return tickets.CommentRef{ID: c.ID, URL: c.HTMLURL}, nil
}

// Link implements tickets.Provider: a child ticket becomes a sub-issue; a pull
// request or a Slack thread is linked in a comment.
func (p *Provider) Link(ctx context.Context, ref tickets.Ref, l tickets.Link) error {
	repo, err := repoOf(ref)
	if err != nil {
		return err
	}
	switch l.Kind {
	case tickets.LinkChild:
		var child issue
		if _, err := repoOf(l.Child); err != nil {
			return err
		}
		if err := p.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", l.Child.Repo, l.Child.Number), nil, &child); err != nil {
			return fmt.Errorf("github: link child %s: %w", l.Child, err)
		}
		if err := p.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/sub_issues", repo, ref.Number), map[string]any{"sub_issue_id": child.ID}, nil); err != nil {
			return fmt.Errorf("github: link child %s to %s: %w", l.Child, ref, err)
		}
		return nil
	case tickets.LinkPullRequest, tickets.LinkSlackThread:
		what := map[tickets.LinkKind]string{tickets.LinkPullRequest: "Pull request", tickets.LinkSlackThread: "Slack thread"}[l.Kind]
		_, err := p.Comment(ctx, ref, fmt.Sprintf("%s: %s", what, l.URL))
		return err
	default:
		return fmt.Errorf("github: unknown link kind %q", l.Kind)
	}
}

// labelInfo gives istari's labels a colour and a description.
func labelInfo(name string) (color, description string) {
	switch {
	case strings.HasPrefix(name, holdPrefix):
		return "d93f0b", "istari: on hold (" + strings.TrimPrefix(name, holdPrefix) + ")"
	case strings.HasPrefix(name, statePrefix):
		return "5319e7", "istari: ticket state " + strings.TrimPrefix(name, statePrefix)
	default:
		return "ededed", ""
	}
}

// ensureLabels creates the labels missing from the repository.
func (p *Provider) ensureLabels(ctx context.Context, repo string, labels []string) error {
	for _, name := range labels {
		key := repo + "\x00" + name
		p.mu.Lock()
		known := p.labels[key]
		p.mu.Unlock()
		if known {
			continue
		}
		err := p.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/labels/%s", repo, url.PathEscape(name)), nil, nil)
		if isStatus(err, http.StatusNotFound) {
			color, desc := labelInfo(name)
			err = p.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/labels", repo), map[string]any{"name": name, "color": color, "description": desc}, nil)
			if isStatus(err, http.StatusUnprocessableEntity) {
				err = nil // created meanwhile
			}
		}
		if err != nil {
			return fmt.Errorf("github: label %s in %s: %w", name, repo, err)
		}
		p.mu.Lock()
		p.labels[key] = true
		p.mu.Unlock()
	}
	return nil
}

// apiError is a non-2xx answer of the API.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s", e.Status, e.Message) }

// Is makes a 404 match tickets.ErrNotFound.
func (e *apiError) Is(target error) bool {
	return target == tickets.ErrNotFound && e.Status == http.StatusNotFound
}

func isStatus(err error, status int) bool {
	e, ok := err.(*apiError)
	return ok && e.Status == status
}

func (p *Provider) do(ctx context.Context, method, path string, body, out any) error {
	_, err := p.request(ctx, method, path, body, out)
	return err
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// doPage GETs one page and returns the path of the next one, if any.
func (p *Provider) doPage(ctx context.Context, path string, out any) (string, error) {
	res, err := p.request(ctx, http.MethodGet, path, nil, out)
	if err != nil {
		return "", err
	}
	m := nextLink.FindStringSubmatch(res.Header.Get("Link"))
	if m == nil {
		return "", nil
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return "", err
	}
	return u.RequestURI(), nil
}

func (p *Provider) request(ctx context.Context, method, path string, body, out any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = strconv.Quote(string(data))
		}
		return nil, &apiError{Status: res.StatusCode, Message: e.Message}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
	}
	return res, nil
}
