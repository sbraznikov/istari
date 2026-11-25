package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sbraznikov/istari/internal/store"
)

// Event types of pull requests (spec section 3.1: the VCS adapter's checks
// and reviews). They are read by polling; webhooks come later (#58).
const (
	EventChecks = "vcs.checks"
	EventReview = "vcs.review"
	// EventClosed: the pull request was merged or closed; its payload says
	// which with merged.
	EventClosed = "vcs.closed"
)

const prKind = "pull_request"

// Tracked is a ticket's pull request as istari last saw it.
type Tracked struct {
	Ticket string `json:"ticket"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Head   string `json:"head,omitempty"` // the commit istari last pushed to it
	// Replaced is the commit that push replaced: its checks are no longer
	// news, even while GitHub still shows it as the head.
	Replaced string `json:"replaced,omitempty"`
	Checks   string `json:"checks,omitempty"`
	SHA      string `json:"sha,omitempty"` // the head commit the checks are of
	Review   string `json:"review,omitempty"`
	Done     bool   `json:"done,omitempty"` // merged or closed: no longer polled

	version int64
}

// Watcher polls tracked pull requests and turns changes of their checks and
// reviews into events.
type Watcher struct {
	client *Client
	store  store.Store
	clock  func() time.Time
}

// NewWatcher returns a watcher.
func NewWatcher(c *Client, s store.Store, clock func() time.Time) *Watcher {
	if clock == nil {
		clock = time.Now
	}
	return &Watcher{client: c, store: s, clock: clock}
}

// Track starts watching a ticket's pull request.
func (w *Watcher) Track(ctx context.Context, ticket, repo string, pr Opened) error {
	data, _ := json.Marshal(Tracked{Ticket: ticket, Repo: repo, Number: pr.Number, URL: pr.URL})
	raw, _ := json.Marshal(map[string]any{"repo": repo, "number": pr.Number})
	_, err := w.store.Append(ctx, store.Event{Type: "vcs.tracked", At: w.clock().UTC(), Actor: "system", Ticket: ticket, Payload: raw},
		store.Change{Kind: prKind, ID: ticket, Data: data})
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("vcs: %s already has a tracked pull request", ticket)
	}
	return err
}

// PullRequestOf returns the ticket's tracked pull request.
func (w *Watcher) PullRequestOf(ctx context.Context, ticket string) (Tracked, error) {
	rec, err := w.store.Get(ctx, prKind, ticket)
	if err != nil {
		return Tracked{}, err
	}
	var t Tracked
	if err := json.Unmarshal(rec.Data, &t); err != nil {
		return Tracked{}, err
	}
	t.version = rec.Version
	return t, nil
}

// SeenMerged reports whether the watch saw the ticket's pull request merged
// (spec 4.8: a human may merge it before istari does). It reads only what
// the watch recorded; false without a tracked pull request.
func (w *Watcher) SeenMerged(ctx context.Context, ticket string) (bool, error) {
	t, err := w.PullRequestOf(ctx, ticket)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil || !t.Done {
		return false, err
	}
	events, err := w.store.Read(ctx, "", store.Filter{Ticket: ticket, TypePrefix: EventClosed}, 0)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != EventClosed {
			continue
		}
		var p struct{ Merged bool }
		if err := json.Unmarshal(events[i].Payload, &p); err != nil {
			return false, err
		}
		return p.Merged, nil
	}
	return false, nil
}

// Merged reports whether the ticket's pull request is merged: as the watch
// recorded it once it saw it closed, else as GitHub has it now. Sync asks
// before it reads a closed issue as a cancel, which may come before the
// watch's poll. false without a tracked pull request.
func (w *Watcher) Merged(ctx context.Context, ticket string) (bool, error) {
	t, err := w.PullRequestOf(ctx, ticket)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if t.Done {
		return w.SeenMerged(ctx, ticket)
	}
	s, err := w.client.Status(ctx, t.Repo, t.Number)
	return s.Merged, err
}

// Pushed records the commit istari pushed to a tracked pull request: the
// checks istari knew are of an older commit, so they are forgotten until CI
// reports on this one.
func (w *Watcher) Pushed(ctx context.Context, ticket, head string) error {
	for {
		t, err := w.PullRequestOf(ctx, ticket)
		if err != nil {
			return err
		}
		if t.SHA != "" {
			t.Replaced = t.SHA
		}
		t.Head, t.Checks, t.SHA = head, "", ""
		data, _ := json.Marshal(t)
		raw, _ := json.Marshal(map[string]any{"number": t.Number, "head": head})
		_, err = w.store.Append(ctx, store.Event{Type: "vcs.pushed", At: w.clock().UTC(), Actor: "system", Ticket: ticket, Payload: raw},
			store.Change{Kind: prKind, ID: ticket, Data: data, IfVersion: t.version})
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
}

// Poll reads every open tracked pull request once.
func (w *Watcher) Poll(ctx context.Context) error {
	recs, err := w.store.List(ctx, prKind)
	if err != nil {
		return err
	}
	var errs []error
	for _, rec := range recs {
		var t Tracked
		if err := json.Unmarshal(rec.Data, &t); err != nil {
			errs = append(errs, err)
			continue
		}
		t.version = rec.Version
		if t.Done {
			continue
		}
		if err := w.poll(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Ticket, err))
		}
	}
	return errors.Join(errs...)
}

func (w *Watcher) poll(ctx context.Context, t Tracked) error {
	s, err := w.client.Status(ctx, t.Repo, t.Number)
	if err != nil {
		return err
	}
	type change struct {
		typ     string
		payload map[string]any
	}
	var changes []change
	stale := s.HeadSHA == t.Replaced && s.HeadSHA != t.Head // GitHub has not caught up with istari's push
	if s.Checks != "" && !stale && (s.Checks != t.Checks || s.HeadSHA != t.SHA) {
		t.Checks, t.SHA = s.Checks, s.HeadSHA
		changes = append(changes, change{EventChecks, map[string]any{"number": t.Number, "checks": s.Checks, "sha": s.HeadSHA}})
	}
	if s.Review != t.Review && !(t.Review == "" && s.Review == ReviewNone) {
		t.Review = s.Review
		changes = append(changes, change{EventReview, map[string]any{"number": t.Number, "review": s.Review, "by": s.By}})
	}
	if (s.Merged || s.Closed) && !t.Done {
		t.Done = true
		changes = append(changes, change{EventClosed, map[string]any{"number": t.Number, "merged": s.Merged}})
	}
	for i, c := range changes {
		raw, _ := json.Marshal(c.payload)
		var more []store.Change
		if i == len(changes)-1 {
			data, _ := json.Marshal(t)
			more = append(more, store.Change{Kind: prKind, ID: t.Ticket, Data: data, IfVersion: t.version})
		}
		if _, err := w.store.Append(ctx, store.Event{Type: c.typ, At: w.clock().UTC(), Actor: "system", Ticket: t.Ticket, Payload: raw}, more...); err != nil {
			return err
		}
	}
	return nil
}
