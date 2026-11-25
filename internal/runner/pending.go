package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/vcs"
)

// The kinds of pending write.
const (
	PendingComment = "comment" // a comment on the ticket, such as a journal entry
	PendingSize    = "size"    // the ticket's size label
	PendingReview  = "review"  // a review on the ticket's pull request
)

// pendingKind is the projection kind of a pending write.
const pendingKind = "pending_write"

// PendingWrite is a tracker write that met an unavailable tracker after the
// adapter's retries: it is kept and delivered later by the daemon (spec
// sections 4.2 and 5).
type PendingWrite struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Ticket    string    `json:"ticket"`
	Body      string    `json:"body,omitempty"` // a comment's text
	Size      string    `json:"size,omitempty"`
	Repo      string      `json:"repo,omitempty"`   // a review: the pull request's repository
	Number    int         `json:"number,omitempty"` // a review: the pull request
	Review    *vcs.Review `json:"review,omitempty"`
	Added    time.Time `json:"added"`
	Next      time.Time `json:"next"` // not tried again before then
	Tries     int       `json:"tries,omitempty"`
	Delivered bool      `json:"delivered,omitempty"`

	version int64
}

// String names the write in the daemon's log, e.g. "comment on gh:o/r#1".
func (w PendingWrite) String() string { return w.Kind + " on " + w.Ticket }

// PendingTracker is what delivering a pending write needs of the tracker.
type PendingTracker interface {
	Comment(ctx context.Context, ref tickets.Ref, body string) (tickets.CommentRef, error)
	SetSize(ctx context.Context, ref tickets.Ref, size string) error
}

// ReviewPublisher is the code host a pending review is published on.
type ReviewPublisher interface {
	PublishReview(ctx context.Context, repo string, n int, r vcs.Review) error
}

// PendingOptions configures NewPendingWrites.
type PendingOptions struct {
	Tracker PendingTracker
	Reviews ReviewPublisher // nil: no code host
	Clock   func() time.Time
	Logf    func(format string, args ...any) // default log.Printf
}

// PendingWrites keeps and replays pending writes.
type PendingWrites struct {
	store store.Store
	opts  PendingOptions
}

// NewPendingWrites returns the pending writes kept in s.
func NewPendingWrites(s store.Store, opts PendingOptions) *PendingWrites {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	return &PendingWrites{store: s, opts: opts}
}

// Add keeps a write to be delivered on the next replay.
func (p *PendingWrites) Add(ctx context.Context, w PendingWrite) error {
	now := p.opts.Clock().UTC()
	w.ID = fmt.Sprintf("%020d-%s", now.UnixNano(), randomHex(4)) // ordered by when it was added
	w.Added, w.Next = now, now
	return p.commit(ctx, w, "tracker.write_pending")
}

// List returns the writes not delivered yet, oldest first.
func (p *PendingWrites) List(ctx context.Context) ([]PendingWrite, error) {
	records, err := p.store.List(ctx, pendingKind)
	if err != nil {
		return nil, err
	}
	var out []PendingWrite
	for _, r := range records {
		var w PendingWrite
		if err := json.Unmarshal(r.Data, &w); err != nil {
			return nil, fmt.Errorf("pending write %s: %w", r.ID, err)
		}
		if !w.Delivered {
			w.version = r.Version
			out = append(out, w)
		}
	}
	return out, nil
}

// Replay delivers the due writes, oldest first. While the tracker is still
// unavailable it stops at the first write and logs when that is tried next;
// a write the tracker refuses for good is dropped and logged.
func (p *PendingWrites) Replay(ctx context.Context) error {
	writes, err := p.List(ctx)
	if err != nil {
		return err
	}
	now := p.opts.Clock().UTC()
	for _, w := range writes {
		if now.Before(w.Next) {
			return nil // keeps the order: nothing later goes first
		}
		err := p.deliver(ctx, w)
		switch {
		case errors.Is(err, tickets.ErrUnavailable):
			w.Tries++
			w.Next = now.Add(replayWait(w.Tries))
			p.opts.Logf("istari: pending write: %s: %v; tried again at %s", w, err, w.Next.Format(time.TimeOnly))
			return p.commit(ctx, w, "tracker.write_retried")
		case err != nil:
			p.opts.Logf("istari: pending write: %s refused, dropped: %v", w, err)
		}
		w.Delivered = true
		if err := p.commit(ctx, w, "tracker.write_delivered"); err != nil {
			return err
		}
	}
	return nil
}

// replayWait is the wait after the tries-th failed replay: one minute,
// doubling, at most a quarter of an hour.
func replayWait(tries int) time.Duration {
	wait := time.Minute << min(tries-1, 4)
	return min(wait, 15*time.Minute)
}

func (p *PendingWrites) deliver(ctx context.Context, w PendingWrite) error {
	ref, err := tickets.ParseRef(w.Ticket)
	if err != nil {
		return err
	}
	switch w.Kind {
	case PendingComment:
		_, err = p.opts.Tracker.Comment(ctx, ref, w.Body)
	case PendingSize:
		err = p.opts.Tracker.SetSize(ctx, ref, w.Size)
	case PendingReview:
		if p.opts.Reviews == nil || w.Review == nil {
			return fmt.Errorf("no code host to publish the review on")
		}
		err = p.opts.Reviews.PublishReview(ctx, w.Repo, w.Number, *w.Review)
	default:
		err = fmt.Errorf("unknown kind %q", w.Kind)
	}
	return err
}

// QueueTracker is the tracker Queuing wraps: what refining and the journal
// write through.
type QueueTracker interface {
	RefineTracker
	Comments(ctx context.Context, ref tickets.Ref) ([]tickets.Comment, error)
}

// Queuing is a tracker whose comments and size labels are kept as pending
// writes when the tracker is unavailable. The error still reports the
// outage, so that the caller can end the run without spending an attempt.
type Queuing struct {
	QueueTracker
	pending *PendingWrites
}

// Queuing wraps t so that its unavailable writes become pending writes.
func (p *PendingWrites) Queuing(t QueueTracker) *Queuing { return &Queuing{QueueTracker: t, pending: p} }

// Comment comments, or keeps the comment for later while the tracker is unavailable.
func (q *Queuing) Comment(ctx context.Context, ref tickets.Ref, body string) (tickets.CommentRef, error) {
	c, err := q.QueueTracker.Comment(ctx, ref, body)
	return c, q.keep(ctx, err, PendingWrite{Kind: PendingComment, Ticket: ref.String(), Body: body})
}

// SetSize sets the size label, or keeps it for later while the tracker is unavailable.
func (q *Queuing) SetSize(ctx context.Context, ref tickets.Ref, size string) error {
	err := q.QueueTracker.SetSize(ctx, ref, size)
	return q.keep(ctx, err, PendingWrite{Kind: PendingSize, Ticket: ref.String(), Size: size})
}

func (q *Queuing) keep(ctx context.Context, err error, w PendingWrite) error {
	if !errors.Is(err, tickets.ErrUnavailable) {
		return err
	}
	if aerr := q.pending.Add(ctx, w); aerr != nil {
		return errors.Join(err, fmt.Errorf("keeping the %s for later: %w", w, aerr))
	}
	return err
}

func (p *PendingWrites) commit(ctx context.Context, w PendingWrite, typ string) error {
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"id": w.ID, "kind": w.Kind})
	_, err = p.store.Append(ctx, store.Event{Type: typ, Actor: "system", Ticket: w.Ticket, Payload: payload},
		store.Change{Kind: pendingKind, ID: w.ID, Data: data, IfVersion: w.version})
	return err
}
