package vcs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sbraznikov/istari/internal/github/ratelimit"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// Event types of pull requests (spec section 3.1: the VCS adapter's checks
// and reviews). They are read by polling; webhooks come later (#58).
const (
	EventChecks = "vcs.checks"
	EventReview = "vcs.review"
	// EventClosed: the pull request was merged or closed; its payload says
	// which with merged.
	EventClosed = "vcs.closed"
	// EventConflict: GitHub reported the pull request in conflict with its
	// base (mergeable false), once per head and base (spec 4.8).
	EventConflict = "vcs.conflicted"
	// EventMergeable: GitHub reported the pull request free of conflicts.
	EventMergeable = "vcs.mergeable_read"
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
	// Mergeable is GitHub's last answer whether the pull request merges into
	// its base, MergeableClean or MergeableDirty, at the head MergeHead and the
	// base MergeBase; "" while GitHub has not answered for istari's head.
	Mergeable string `json:"mergeable,omitempty"`
	MergeHead string `json:"merge_head,omitempty"`
	MergeBase string `json:"merge_base,omitempty"`

	version int64
}

// What GitHub says about merging a pull request into its base.
const (
	MergeableClean = "clean"
	MergeableDirty = "dirty" // conflicts
)

// Watcher polls tracked pull requests and turns changes of their checks and
// reviews into events.
type Watcher struct {
	client  *Client
	store   store.Store
	clock   func() time.Time
	journal *journal.Journal
}

// WatcherOptions configures a Watcher.
type WatcherOptions struct {
	Clock func() time.Time // nil: time.Now
	// Journal is where the watcher records a CI failure entry when it sees a
	// red head (spec 4.3); nil: a red head is only a vcs.checks event.
	Journal *journal.Journal
}

// NewWatcher returns a watcher.
func NewWatcher(c *Client, s store.Store, opts WatcherOptions) *Watcher {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Watcher{client: c, store: s, clock: opts.Clock, journal: opts.Journal}
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
// checks and the mergeable istari knew are of an older commit, so they are
// forgotten until GitHub reports on this one.
func (w *Watcher) Pushed(ctx context.Context, ticket, head string) error {
	for {
		t, err := w.PullRequestOf(ctx, ticket)
		if err != nil {
			return err
		}
		if old := cmp.Or(t.SHA, t.MergeHead); old != "" {
			t.Replaced = old
		}
		t.Head, t.Checks, t.SHA = head, "", ""
		t.Mergeable, t.MergeHead, t.MergeBase = "", "", ""
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
	ctx = ratelimit.WithKind(ctx, ratelimit.KindPRWatch)
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
	var failed error                                        // a CI failure entry that could not be recorded; the event goes on regardless
	if s.Checks != "" && !stale && (s.Checks != t.Checks || s.HeadSHA != t.SHA) {
		t.Checks, t.SHA = s.Checks, s.HeadSHA
		changes = append(changes, change{EventChecks, map[string]any{"number": t.Number, "checks": s.Checks, "sha": s.HeadSHA}})
		if s.Checks == "failure" {
			failed = w.recordFailure(ctx, t)
		}
	}
	if s.Review != t.Review && !(t.Review == "" && s.Review == ReviewNone) {
		t.Review = s.Review
		changes = append(changes, change{EventReview, map[string]any{"number": t.Number, "review": s.Review, "by": s.By}})
	}
	if m, ok := mergeable(s, t, stale); ok {
		t.Mergeable, t.MergeHead, t.MergeBase = m, s.HeadSHA, s.BaseSHA
		typ := EventMergeable
		if m == MergeableDirty {
			typ = EventConflict
		}
		changes = append(changes, change{typ, map[string]any{"number": t.Number, "head": s.HeadSHA, "base": s.BaseSHA, "state": s.MergeableState}})
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
			return errors.Join(failed, err)
		}
	}
	return failed
}

// recordFailure appends a CI failure entry for the tracked head, once per
// commit: the failed jobs' logs, or why they could not be read. It comes
// before the vcs.checks event, so the rework brief has it.
func (w *Watcher) recordFailure(ctx context.Context, t Tracked) error {
	if w.journal == nil {
		return nil
	}
	ref, err := tickets.ParseRef(t.Ticket)
	if err != nil {
		return err
	}
	entries, err := w.journal.Stored(ctx, ref)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Kind == journal.CI && e.CI.Commit == t.SHA {
			return nil // recorded by an earlier poll whose event did not get through
		}
	}
	f, err := w.client.FailureLog(ctx, t.Repo, t.SHA)
	ci := &journal.CIEntry{Check: f.Check, Commit: t.SHA, URL: f.URL, Output: f.Output, Cut: f.Cut}
	if err != nil {
		ci.Error = err.Error()
	}
	if ci.Check == "" {
		ci.Check = "CI"
	}
	return w.journal.Append(ctx, ref, journal.Entry{Kind: journal.CI, Actor: "system", CI: ci})
}

// mergeable reads GitHub's answer whether the pull request merges into its
// base; ok is false when there is nothing new: GitHub has not computed it yet
// (null), the answer is of a head istari has replaced, or it is the one
// recorded for this head and base.
func mergeable(s Status, t Tracked, stale bool) (string, bool) {
	if s.Mergeable == nil || stale || s.Merged || s.Closed {
		return "", false
	}
	m := MergeableClean
	if !*s.Mergeable {
		m = MergeableDirty
	}
	if m == t.Mergeable && s.HeadSHA == t.MergeHead && s.BaseSHA == t.MergeBase {
		return "", false
	}
	return m, true
}
