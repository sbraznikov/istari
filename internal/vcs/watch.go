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
)

const prKind = "pull_request"

// Tracked is a ticket's pull request as istari last saw it.
type Tracked struct {
	Ticket string `json:"ticket"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Checks string `json:"checks,omitempty"`
	Review string `json:"review,omitempty"`
	Done   bool   `json:"done,omitempty"` // merged or closed: no longer polled

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
func (w *Watcher) Track(ctx context.Context, ticket, repo string, n int) error {
	data, _ := json.Marshal(Tracked{Ticket: ticket, Repo: repo, Number: n})
	raw, _ := json.Marshal(map[string]any{"repo": repo, "number": n})
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
	if s.Checks != "" && s.Checks != t.Checks {
		t.Checks = s.Checks
		changes = append(changes, change{EventChecks, map[string]any{"number": t.Number, "checks": s.Checks, "sha": s.HeadSHA}})
	}
	if s.Review != "" && s.Review != t.Review {
		t.Review = s.Review
		changes = append(changes, change{EventReview, map[string]any{"number": t.Number, "review": s.Review, "by": s.By}})
	}
	if s.Merged || s.Closed {
		t.Done = true
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
