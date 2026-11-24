package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
)

// DefaultSyncInterval is how often the tracker is polled (maintainer
// decision on #16: every 5 minutes, and on demand with istari sync).
const DefaultSyncInterval = 5 * time.Minute

// Event types of the ticket sync (spec section 8: sync.*).
const (
	EventSyncPushed   = "sync.pushed"   // istari wrote a ticket's state to the tracker
	EventSyncObserved = "sync.observed" // the tracker already had the state istari has
	EventSyncReverted = "sync.reverted" // a human's label change was undone
	EventSyncComment  = "sync.comment"  // a human commented on a ticket
)

// SyncProvider is a tracker the sync can work with.
type SyncProvider interface {
	Provider
	History
	Access
}

// SyncRepo is one repository of a project whose tickets are synced.
type SyncRepo struct {
	Repo    string // owner/repo
	Project string
}

// SyncOptions configures a Syncer.
type SyncOptions struct {
	Clock func() time.Time
}

// SyncReport counts what one sync did.
type SyncReport struct {
	Created      int `json:"created"`
	Pushed       int `json:"pushed"`
	Transitioned int `json:"transitioned"`
	Reverted     int `json:"reverted"`
	Comments     int `json:"comments"`
}

// Syncer keeps the tracker and istari's tickets in step (spec section 3.1,
// Ticket Sync). For every ticket it remembers the state it last wrote to or
// accepted from the tracker (the base). Then:
//
//   - tracker as the base, istari changed: istari's change is written out;
//   - tracker changed: a human edited it, which is a request for a
//     transition. It is applied if the table allows it and the human has
//     write access; otherwise the labels are restored and a comment says why;
//   - tracker equal to istari: nothing to do. This is how istari's own writes
//     never come back as requests (echo suppression), even when istari and
//     a human use the same tracker account.
type Syncer struct {
	provider SyncProvider
	tickets  *core.Tickets
	store    store.Store
	repos    []SyncRepo
	opts     SyncOptions

	mu      sync.Mutex           // one sync at a time
	updated map[string]time.Time // per ticket: the tracker's update time whose comments were read
}

// NewSyncer returns a Syncer.
func NewSyncer(p SyncProvider, t *core.Tickets, s store.Store, repos []SyncRepo, opts SyncOptions) *Syncer {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Syncer{provider: p, tickets: t, store: s, repos: repos, opts: opts, updated: map[string]time.Time{}}
}

// Run syncs at once and then every interval until ctx ends; wake asks for a
// sync now.
func (s *Syncer) Run(ctx context.Context, every time.Duration, wake <-chan struct{}) {
	if every <= 0 {
		every = DefaultSyncInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if _, err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			log.Printf("istari: ticket sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

// Sync reconciles every repository once. Errors on single tickets do not stop
// the others; they are returned together, and the next sync repairs them.
func (s *Syncer) Sync(ctx context.Context) (SyncReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var report SyncReport
	var errs []error
	for _, repo := range s.repos {
		if err := s.syncRepo(ctx, repo, &report); err != nil {
			errs = append(errs, fmt.Errorf("sync %s: %w", repo.Repo, err))
		}
	}
	return report, errors.Join(errs...)
}

func (s *Syncer) syncRepo(ctx context.Context, repo SyncRepo, report *SyncReport) error {
	open, err := s.provider.List(ctx, Query{Repo: repo.Repo})
	if err != nil {
		return err
	}
	var errs []error
	seen := map[string]bool{}
	for _, t := range open {
		seen[t.Ref.String()] = true
		if err := s.reconcile(ctx, repo, t, report); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Ref, err))
		}
	}
	// Tickets istari believes open but the tracker no longer lists as open:
	// closed by a human, or gone.
	all, err := s.tickets.List(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, tk := range all {
		ref, err := ParseRef(tk.Ref)
		if err != nil || ref.Repo != repo.Repo || seen[tk.Ref] {
			continue
		}
		base, _, err := s.base(ctx, tk.Ref)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if base.Closed && finished(tk.State) {
			continue // closed on both sides
		}
		t, err := s.provider.Get(ctx, ref)
		if errors.Is(err, ErrNotFound) {
			continue // deleted or moved: nothing to write to
		}
		if err == nil {
			err = s.reconcile(ctx, repo, t, report)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tk.Ref, err))
		}
	}
	return errors.Join(errs...)
}

// side is a ticket's state as one side has it.
type side struct {
	State  core.TicketState `json:"state"`
	Hold   string           `json:"hold,omitempty"`
	Closed bool             `json:"closed"`
}

func finished(s core.TicketState) bool { return s == core.TicketDone || s == core.TicketCanceled }

func localSide(tk core.Ticket) side {
	return side{State: tk.State, Hold: tk.Hold, Closed: finished(tk.State)}
}

const baseKind = "synced"

func (s *Syncer) base(ctx context.Context, ref string) (side, int64, error) {
	rec, err := s.store.Get(ctx, baseKind, ref)
	if errors.Is(err, store.ErrNotFound) {
		return side{}, 0, nil
	}
	if err != nil {
		return side{}, 0, err
	}
	var b side
	if err := json.Unmarshal(rec.Data, &b); err != nil {
		return side{}, 0, err
	}
	return b, rec.Version, nil
}

// record appends a sync event together with the new base.
func (s *Syncer) record(ctx context.Context, typ, actor string, tk core.Ticket, base side, version int64, payload map[string]any) error {
	data, _ := json.Marshal(base)
	raw, _ := json.Marshal(payload)
	_, err := s.store.Append(ctx, store.Event{Type: typ, At: s.opts.Clock(), Actor: actor, Project: tk.Project, Ticket: tk.Ref, Payload: raw},
		store.Change{Kind: baseKind, ID: tk.Ref, Data: data, IfVersion: version})
	return err
}

func (s *Syncer) reconcile(ctx context.Context, repo SyncRepo, t Ticket, report *SyncReport) error {
	ref := t.Ref.String()
	tk, err := s.tickets.Get(ctx, ref)
	var coreErr *core.Error
	if errors.As(err, &coreErr) && coreErr.Code == core.CodeNotFound {
		tk, err = s.tickets.CreateWith(ctx, "human:"+t.Author, core.NewTicket{Ref: ref, Project: repo.Project, Title: t.Title, Author: t.Author, URL: t.URL})
		if err == nil {
			report.Created++
		}
	}
	if err != nil {
		return err
	}
	base, version, err := s.base(ctx, ref)
	if err != nil {
		return err
	}
	tracker := side{State: t.State.State, Hold: t.State.Hold, Closed: t.Closed}
	local := localSide(tk)
	switch {
	case tracker == local:
		if base != local {
			if err := s.record(ctx, EventSyncObserved, "system", tk, local, version, map[string]any{"state": local.State}); err != nil {
				return err
			}
		}
	case tracker == base || tracker.State == "":
		// The tracker still has what istari last wrote, or no state at all.
		if err := s.push(ctx, tk, version); err != nil {
			return err
		}
		report.Pushed++
	default:
		if err := s.request(ctx, repo, t, tk, tracker, version, report); err != nil {
			return err
		}
	}
	return s.comments(ctx, t, tk, report)
}

// push writes istari's state of tk to the tracker.
func (s *Syncer) push(ctx context.Context, tk core.Ticket, version int64) error {
	ref, err := ParseRef(tk.Ref)
	if err != nil {
		return err
	}
	if err := s.provider.SetState(ctx, ref, State{State: tk.State, Hold: tk.Hold}); err != nil {
		return err
	}
	local := localSide(tk)
	return s.record(ctx, EventSyncPushed, "system", tk, local, version, map[string]any{"state": local.State, "hold": local.Hold})
}

// request treats the tracker's differing state as a human's request.
func (s *Syncer) request(ctx context.Context, repo SyncRepo, t Ticket, tk core.Ticket, tracker side, version int64, report *SyncReport) error {
	var target core.TicketState
	var kind, label, change string
	switch {
	case tracker.State != tk.State:
		target, kind, label = tracker.State, "labeled", "istari:"+string(tracker.State)
		change = "the label `" + label + "`"
	case tracker.Closed && !finished(tk.State):
		target, kind, change = core.TicketCanceled, "closed", "closing the issue"
	case !tracker.Closed && tk.State == core.TicketCanceled:
		target, kind, change = core.TicketNew, "reopened", "reopening the issue"
	case !tracker.Closed && tk.State == core.TicketDone:
		return s.revert(ctx, t, tk, version, "", "reopening the issue", "a done ticket is not reopened; file a new ticket that links this one", report)
	default:
		return s.revert(ctx, t, tk, version, "", "the hold label", "holds are set by istari; use istari ticket pause, resume or retry", report)
	}
	who, err := s.provider.LastActor(ctx, t.Ref, kind, label)
	if err != nil {
		return err
	}
	trigger, ok := core.TriggerTo(tk.State, target)
	if !ok {
		return s.revert(ctx, t, tk, version, who, change, fmt.Sprintf("a ticket in `%s` cannot go to `%s`", tk.State, target), report)
	}
	allowed, err := s.provider.CanWrite(ctx, repo.Repo, who)
	if err != nil {
		return err
	}
	if !allowed {
		return s.revert(ctx, t, tk, version, who, change, fmt.Sprintf("@%s has no write access to %s", who, repo.Repo), report)
	}
	next, err := s.tickets.Fire(ctx, "human:"+who, tk.Ref, trigger)
	if errors.As(err, new(*core.Error)) {
		return s.revert(ctx, t, tk, version, who, change, err.Error(), report)
	}
	if err != nil {
		return err
	}
	report.Transitioned++
	if err := s.push(ctx, next, version); err != nil {
		return err
	}
	report.Pushed++
	return nil
}

// revert restores istari's state in the tracker and says why in a comment.
func (s *Syncer) revert(ctx context.Context, t Ticket, tk core.Ticket, version int64, who, change, reason string, report *SyncReport) error {
	if err := s.provider.SetState(ctx, t.Ref, State{State: tk.State, Hold: tk.Hold}); err != nil {
		return err
	}
	by := ""
	if who != "" {
		by = " by @" + who
	}
	body := fmt.Sprintf("istari undid %s%s: %s. The ticket stays in `%s`.", change, by, reason, tk.State)
	if _, err := s.provider.Comment(ctx, t.Ref, body); err != nil {
		return err
	}
	report.Reverted++
	actor := "system"
	if who != "" {
		actor = "human:" + who
	}
	return s.record(ctx, EventSyncReverted, actor, tk, localSide(tk), version, map[string]any{"change": change, "reason": reason})
}

const commentKind = "comment"

// comments delivers each human comment once as a sync.comment event.
func (s *Syncer) comments(ctx context.Context, t Ticket, tk core.Ticket, report *SyncReport) error {
	ref := t.Ref.String()
	if t.Comments == 0 || (!t.Updated.IsZero() && s.updated[ref].Equal(t.Updated)) {
		return nil
	}
	comments, err := s.provider.Comments(ctx, t.Ref)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if c.Own {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"id": c.ID, "author": c.Author, "body": c.Body, "url": c.URL})
		_, err := s.store.Append(ctx, store.Event{Type: EventSyncComment, At: s.opts.Clock(), Actor: "human:" + c.Author, Project: tk.Project, Ticket: ref, Payload: raw},
			store.Change{Kind: commentKind, ID: ref + "/" + strconv.FormatInt(c.ID, 10), Data: json.RawMessage(`{}`)})
		if errors.Is(err, store.ErrConflict) {
			continue // delivered before
		}
		if err != nil {
			return err
		}
		report.Comments++
	}
	s.updated[ref] = t.Updated
	return nil
}
