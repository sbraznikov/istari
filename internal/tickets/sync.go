package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
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
	// Trusted are the logins admitted without a human, who may also admit
	// others' tickets; empty means everyone with write access (spec 4.5).
	Trusted []string
	// LabelOnly reports whether the project admits tickets only by a trusted
	// member's manual admission or by inheritance from an admitted ticket, so
	// that a trusted author alone is not enough (admission = "label", spec 4.5).
	LabelOnly bool
}

// authorAdmits tells whether the ticket's author alone admits it: the
// project admits by author and the author is trusted.
func (s *Syncer) authorAdmits(ctx context.Context, repo SyncRepo, author string) (bool, error) {
	if repo.LabelOnly {
		return false, nil
	}
	return s.trusted(ctx, repo, author)
}

// Standards checks tickets against their repository's ticket standard.
type Standards interface {
	Check(ctx context.Context, repo string, t Ticket) (Conformance, error)
}

// Conformance is the result of a structural check.
type Conformance struct {
	Deviations []string // empty: the ticket meets the standard
	Hash       string   // digest of the required sections, for T15
	// Priority is the priority the ticket's table states, "" when it states
	// none: it becomes the stored priority on admission (spec 4.5).
	Priority string
}

// SyncOptions configures a Syncer.
type SyncOptions struct {
	Clock func() time.Time
	// Standards turns on admission (T2) and the re-check on edit (T15);
	// without it the sync only mirrors states.
	Standards Standards
	// Needs reads the tickets a ticket's text depends on (spec 4.9); nil
	// leaves the needs alone.
	Needs func(body, repo string) []string
	// Parent reads the ticket a ticket's text names as its parent; istari
	// recover gives split parents their children with it. nil: none.
	Parent func(body, repo string) string
	// Note adds a note to the ticket's journal; nil: the sync notes nothing.
	Note func(ctx context.Context, ref Ref, text string) error
	// Comment records a trusted member's comment on a ticket in work as a
	// journal entry (spec 4.5 and 4.6), once; nil: comments are only events.
	Comment func(ctx context.Context, ref Ref, c Comment) error
	// Merged tells whether a ticket's pull request is merged (spec 4.8): an
	// issue its merge closed is not a cancel. fresh asks the code host when
	// the PR watch has not seen the pull request closed yet; otherwise only
	// what the watch recorded counts. nil: no pull request is ever merged.
	Merged func(ctx context.Context, ref string, fresh bool) (bool, error)
}

// CodeNotTrusted refuses an admission by someone who is not trusted.
const CodeNotTrusted = "not_trusted"

// SyncReport counts what one sync did.
type SyncReport struct {
	Created      int `json:"created"`
	Pushed       int `json:"pushed"`
	Transitioned int `json:"transitioned"`
	Reverted     int `json:"reverted"`
	Comments     int `json:"comments"`
	// Unknown are tickets istari does not have whose labels say they are
	// past ready: istari lost them, and istari recover takes them back.
	Unknown int `json:"unknown,omitempty"`
	// Recovered are the tickets istari recover took back; Skipped, the
	// lost tickets it did not, and why.
	Recovered int      `json:"recovered,omitempty"`
	Skipped   []string `json:"skipped,omitempty"`
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

// Run syncs at once and then every interval until ctx ends. istari sync calls
// Sync directly.
func (s *Syncer) Run(ctx context.Context, every time.Duration) {
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
		err := s.reconcile(ctx, repo, t, report)
		if err == nil {
			err = s.closedNeeds(ctx, t.Ref.String(), report)
		}
		if err != nil {
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
		if base.side.Closed && finished(tk.State) {
			repaired, err := s.repairMerged(ctx, tk)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", tk.Ref, err))
			}
			if !repaired {
				continue // closed on both sides
			}
			report.Transitioned++
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

// EventAdmissionRequested: a trusted member asked to admit a ticket that
// falls short of the standard; it is refined, then admitted.
const EventAdmissionRequested = "ticket.admission_requested"

// Refinable tells whether the refiner may work on a ticket (spec 4.5): it
// is admitted once it meets the standard, because its author is trusted, an
// agent created it while working on an admitted ticket, or a trusted member
// asked to admit it. Nobody else's ticket costs an agent run.
func (s *Syncer) Refinable(ctx context.Context, refText string) (bool, error) {
	tk, err := s.tickets.Get(ctx, refText)
	if err != nil {
		return false, err
	}
	if tk.BudgetOf != "" {
		return true, nil
	}
	base, _, err := s.base(ctx, refText)
	if err != nil {
		return false, err
	}
	ref, err := ParseRef(refText)
	if err != nil {
		return false, nil
	}
	repo, ok := s.repoOf(ref)
	if !ok {
		return false, nil
	}
	if sponsored, err := s.sponsored(ctx, repo, base); err != nil || sponsored {
		return sponsored, err
	}
	return s.authorAdmits(ctx, repo, tk.Author)
}

// sponsored tells whether a trusted member asked to admit the ticket and is
// still trusted.
func (s *Syncer) sponsored(ctx context.Context, repo SyncRepo, base baseRecord) (bool, error) {
	if base.Sponsor == "" {
		return false, nil
	}
	return s.trusted(ctx, repo, base.Sponsor)
}

// lost tells whether a ticket istari does not have but its tracker labels as
// state was worked on by istari: past ready.
func lost(state core.TicketState) bool {
	return state != "" && state != core.TicketNew && state != core.TicketReady
}

// Recover takes back the tickets istari lost with its database (spec
// section 4.6, istari recover): every open ticket labelled past ready that
// istari does not have gets the state and hold of its labels, and its
// journal is restored from its comments by restore; the closed tickets it
// needs come back as done or canceled. A state label counts only when
// istari itself or a trusted member set it: anyone who can label could set
// one. The tracker is not changed.
func (s *Syncer) Recover(ctx context.Context, restore func(context.Context, Ref) error) (SyncReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var report SyncReport
	self, err := s.provider.Self(ctx)
	if err != nil {
		return report, err
	}
	var errs []error
	for _, repo := range s.repos {
		open, err := s.provider.List(ctx, Query{Repo: repo.Repo})
		if err != nil {
			errs = append(errs, fmt.Errorf("recover %s: %w", repo.Repo, err))
			continue
		}
		// The tickets istari worked on: only they can have been split.
		split := map[string]bool{}
		for _, t := range open {
			if lost(t.State.State) {
				split[t.Ref.String()] = true
			}
		}
		for _, t := range open {
			if !lost(t.State.State) {
				continue
			}
			if _, err := s.tickets.Get(ctx, t.Ref.String()); err == nil {
				continue // istari has it
			}
			if err := s.recoverOne(ctx, repo, t, self, split, &report); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.Ref, err))
				continue
			}
			if err := restore(ctx, t.Ref); err != nil {
				errs = append(errs, fmt.Errorf("%s: the journal: %w", t.Ref, err))
			}
		}
		if err := s.recoverChildren(ctx, repo, &report); err != nil {
			errs = append(errs, fmt.Errorf("recover %s: the children: %w", repo.Repo, err))
		}
	}
	return report, errors.Join(errs...)
}

// recoverChildren gives every ticket that was split its children back: the
// tickets, open or closed, whose text names it as their parent. They share
// its budget; closed ones come back as done or canceled.
func (s *Syncer) recoverChildren(ctx context.Context, repo SyncRepo, report *SyncReport) error {
	all, err := s.provider.List(ctx, Query{Repo: repo.Repo, Closed: true})
	if err != nil {
		return err
	}
	children := map[string][]string{}
	for _, t := range all {
		parent := s.parentOf(t, repo)
		if parent == "" {
			continue
		}
		if p, err := s.tickets.Get(ctx, parent); err != nil || !lost(p.State) {
			continue // not a ticket istari split
		}
		ref := t.Ref.String()
		if _, err := s.tickets.Get(ctx, ref); err != nil {
			if !t.Closed || !finished(t.State.State) {
				continue // an open child is recovered on its own, or skipped
			}
			nt := core.NewTicket{Ref: ref, Project: repo.Project, Title: t.Title, Author: t.Author, URL: t.URL, BudgetOf: parent}
			if _, err := s.tickets.Recover(ctx, "system", nt, t.State.State, ""); err != nil {
				return err
			}
			if err := s.reconcile(ctx, repo, t, report); err != nil {
				return err
			}
		}
		children[parent] = append(children[parent], ref)
	}
	for parent, refs := range children {
		p, err := s.tickets.Get(ctx, parent)
		if err != nil || len(p.Children) > 0 {
			continue
		}
		slices.SortFunc(refs, func(a, b string) int {
			ra, _ := ParseRef(a)
			rb, _ := ParseRef(b)
			return ra.Number - rb.Number
		})
		if _, err := s.tickets.SetChildren(ctx, "system", parent, refs); err != nil {
			return err
		}
	}
	return nil
}

// recoverOne takes one lost ticket back, then the closed tickets it needs.
func (s *Syncer) recoverOne(ctx context.Context, repo SyncRepo, t Ticket, self string, split map[string]bool, report *SyncReport) error {
	label := "istari:" + string(t.State.State)
	by, err := s.provider.LastActor(ctx, t.Ref, "labeled", label)
	if err != nil {
		return err
	}
	if !strings.EqualFold(by, self) {
		ok, err := s.trusted(ctx, repo, by)
		if err != nil {
			return err
		}
		if !ok {
			report.Skipped = append(report.Skipped, fmt.Sprintf("%s: %s was set by %s, who is not trusted", t.Ref, label, or(by, "nobody")))
			return nil
		}
	}
	nt := core.NewTicket{Ref: t.Ref.String(), Project: repo.Project, Title: t.Title, Author: t.Author, URL: t.URL}
	if parent := s.parentOf(t, repo); parent != "" && split[parent] {
		nt.BudgetOf = parent // a child of a split inherits its parent's admission (spec 4.5)
	}
	if _, err := s.tickets.Recover(ctx, "system", nt, t.State.State, t.State.Hold); err != nil {
		return err
	}
	report.Recovered++
	if err := s.reconcile(ctx, repo, t, report); err != nil {
		return err
	}
	return s.closedNeeds(ctx, t.Ref.String(), report)
}

// closedNeeds takes in the closed tickets that ticket ref needs and istari
// does not have: an issue closed before istari was set up, or lost with its
// database. Each comes in as done or canceled, so the hold waiting_dependency
// on ref can end (spec section 4.1, T3); an open need is synced on its own.
// A ticket istari does not have is skipped.
func (s *Syncer) closedNeeds(ctx context.Context, ref string, report *SyncReport) error {
	tk, err := s.tickets.Get(ctx, ref)
	var coreErr *core.Error
	if errors.As(err, &coreErr) && coreErr.Code == core.CodeNotFound {
		return nil // left for recover
	}
	if err != nil {
		return err
	}
	for _, need := range tk.Needs {
		needRef, err := ParseRef(need)
		if err != nil {
			continue
		}
		if _, err := s.tickets.Get(ctx, need); err == nil {
			continue
		}
		c, err := s.provider.Get(ctx, needRef)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !c.Closed {
			continue // an open need is recovered or synced on its own
		}
		cr, ok := s.repoOf(needRef)
		if !ok {
			continue
		}
		state := c.State.State
		if !finished(state) {
			state = closedState(c.ClosedAs)
		}
		if _, err := s.tickets.Recover(ctx, "system", core.NewTicket{Ref: need, Project: cr.Project, Title: c.Title, Author: c.Author, URL: c.URL}, state, ""); err != nil {
			return err
		}
		if err := s.reconcile(ctx, cr, c, report); err != nil {
			return err
		}
	}
	return nil
}

// closedState is the state of a closed issue without an istari state label,
// from how it was closed, the reverse of spec section 4.1 (done closes as
// completed, canceled as not planned): not planned or a duplicate is
// canceled; completed, or no reason (an issue closed before trackers recorded
// one), is done.
func closedState(closedAs string) core.TicketState {
	switch closedAs {
	case "not_planned", "duplicate":
		return core.TicketCanceled
	}
	return core.TicketDone
}

// parentOf is the ticket t's text names as its parent, or "".
func (s *Syncer) parentOf(t Ticket, repo SyncRepo) string {
	if s.opts.Parent == nil {
		return ""
	}
	return s.opts.Parent(t.Body, repo.Repo)
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
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

// baseRecord is what the sync remembers per ticket.
type baseRecord struct {
	side
	// FirstComment is the id of the last comment that existed when istari
	// first saw the ticket; earlier comments are not delivered as events.
	FirstComment int64 `json:"first_comment,omitempty"`
	// Hash is the digest of the required sections when the ticket was
	// admitted; an edit that changes it in ready is T15.
	Hash string `json:"hash,omitempty"`
	// Priority is the priority label the tracker had at the last sync.
	Priority string `json:"priority,omitempty"`
	// Sponsor is the trusted member who asked to admit the ticket while it
	// fell short of the standard: it is refined, then admitted by them.
	Sponsor string `json:"sponsor,omitempty"`
	// PriorityNoted is the pair "stored/table" of priorities whose
	// difference the journal last noted, so it says so once.
	PriorityNoted string `json:"priority_noted,omitempty"`
}

// base returns the ticket's base record; version 0 means istari has never
// synced this ticket.
func (s *Syncer) base(ctx context.Context, ref string) (baseRecord, int64, error) {
	rec, err := s.store.Get(ctx, baseKind, ref)
	if errors.Is(err, store.ErrNotFound) {
		return baseRecord{}, 0, nil
	}
	if err != nil {
		return baseRecord{}, 0, err
	}
	var b baseRecord
	if err := json.Unmarshal(rec.Data, &b); err != nil {
		return baseRecord{}, 0, err
	}
	return b, rec.Version, nil
}

// sync is the state of one ticket during a reconcile.
type syncing struct {
	repo    SyncRepo
	t       Ticket      // the tracker's ticket
	tk      core.Ticket // istari's ticket
	base    baseRecord
	version int64
	report  *SyncReport
	wrote   bool    // istari wrote its labels to the tracker in this reconcile
	refused *change // a priority change to revert, and why
	reason  string
}

// record appends a sync event together with the ticket's new base.
func (s *Syncer) record(ctx context.Context, x *syncing, typ, actor string, tk core.Ticket, payload map[string]any) error {
	base := x.base
	base.side = localSide(tk)
	data, _ := json.Marshal(base)
	raw, _ := json.Marshal(payload)
	_, err := s.store.Append(ctx, store.Event{Type: typ, At: s.opts.Clock(), Actor: actor, Project: tk.Project, Ticket: tk.Ref, Payload: raw},
		store.Change{Kind: baseKind, ID: tk.Ref, Data: data, IfVersion: x.version})
	if err == nil {
		x.base, x.tk = base, tk
		x.version++
	}
	return err
}

func (s *Syncer) reconcile(ctx context.Context, repo SyncRepo, t Ticket, report *SyncReport) error {
	ref := t.Ref.String()
	tk, err := s.tickets.Get(ctx, ref)
	var coreErr *core.Error
	if errors.As(err, &coreErr) && coreErr.Code == core.CodeNotFound {
		if lost(t.State.State) {
			report.Unknown++ // istari lost it: istari recover takes it back
			return nil
		}
		nt := core.NewTicket{Ref: ref, Project: repo.Project, Title: t.Title, Author: t.Author, URL: t.URL}
		if tk, err = s.tickets.CreateWith(ctx, "human:"+t.Author, nt); err == nil {
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
	x := &syncing{repo: repo, t: t, tk: tk, base: base, version: version, report: report}
	if version == 0 && t.Comments > 0 {
		// Comments from before istari saw the ticket are history, not news.
		comments, err := s.provider.Comments(ctx, t.Ref)
		if err != nil {
			return err
		}
		for _, c := range comments {
			x.base.FirstComment = max(x.base.FirstComment, c.ID)
		}
	}
	if err := s.priorityRequest(ctx, x); err != nil {
		return err
	}
	if err := s.reconcileState(ctx, x); err != nil {
		return err
	}
	if err := s.settlePriority(ctx, x); err != nil {
		return err
	}
	if err := s.size(ctx, x); err != nil {
		return err
	}
	if s.opts.Needs != nil {
		tk, err := s.tickets.SetNeeds(ctx, "system", x.tk.Ref, s.opts.Needs(x.t.Body, x.repo.Repo))
		if err != nil {
			return err
		}
		x.tk = tk
	}
	if err := s.comments(ctx, x); err != nil {
		return err
	}
	return s.admission(ctx, x)
}

func (s *Syncer) reconcileState(ctx context.Context, x *syncing) error {
	local := localSide(x.tk)
	tracker := side{State: x.t.State.State, Hold: x.t.State.Hold, Closed: x.t.Closed}
	if len(x.t.StateLabels) > 1 {
		// A label added next to the old one: the request is the one that
		// differs from istari's state.
		var other []core.TicketState
		for _, st := range x.t.StateLabels {
			if st != local.State && !slices.Contains(other, st) {
				other = append(other, st)
			}
		}
		switch len(other) {
		case 0:
			tracker.State = local.State
		case 1:
			tracker.State = other[0]
		default:
			return s.revert(ctx, x, change{what: "the state labels"}, "the issue carries several state labels, so the requested one is unclear")
		}
	}
	switch {
	case tracker == local && len(x.t.StateLabels) <= 1:
		if x.version == 0 || x.base.side != local {
			return s.record(ctx, x, EventSyncObserved, "system", x.tk, map[string]any{"state": local.State})
		}
		return nil
	case tracker.State == "" && x.version > 0:
		return s.revert(ctx, x, change{what: "removing the state label"}, "istari keeps exactly one state label on its tickets")
	case tracker.State == "" && tracker.Closed && !local.Closed:
		// Closed before istari labelled it: a request to cancel.
		return s.request(ctx, x, tracker)
	case tracker == x.base.side || tracker.State == "" || tracker == local,
		tracker.Closed && local.State == core.TicketDone && tracker.State == x.base.side.State:
		// The tracker still has what istari last wrote, or no state yet, or
		// only stray extra labels, or only the close of the issue that the
		// merge of its PR made after istari finished the ticket (spec 4.8):
		// istari's state goes out.
		if err := s.push(ctx, x, x.tk); err != nil {
			return err
		}
		x.report.Pushed++
		return nil
	default:
		return s.request(ctx, x, tracker)
	}
}

// labelValues returns the values of the ticket's labels prefix+value whose
// value is one of values.
func labelValues(labels []string, prefix string, values []string) []string {
	var out []string
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, prefix); ok && slices.Contains(values, v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// requested returns the one label value that differs from istari's: a human
// who adds a label next to the old one leaves both. Several differing values
// make the request unclear.
func requested(values []string, local string) (value string, several bool) {
	other := slices.DeleteFunc(slices.Clone(values), func(v string) bool { return v == local })
	switch len(other) {
	case 0:
		return "", false
	case 1:
		return other[0], false
	default:
		return "", true
	}
}

// priorityRequest handles a priority label a human set or changed since the
// last sync: a request to change the priority. It is applied when they are
// the ticket's author or have write access (spec 4.9: the author or a project
// member sets it); otherwise it is kept for settlePriority to revert. It runs
// before the state is reconciled, so every label write carries the outcome.
func (s *Syncer) priorityRequest(ctx context.Context, x *syncing) error {
	tracker, several := requested(labelValues(x.t.Labels, "priority:", core.Priorities()), x.tk.Priority)
	if several {
		x.refused, x.reason = &change{what: "the priority labels"}, "the issue carries several priority labels, so the requested one is unclear"
		return nil
	}
	if tracker == "" || tracker == x.base.Priority {
		return nil
	}
	c := change{what: "the label `priority:" + tracker + "`"}
	who, err := s.provider.LastActor(ctx, x.t.Ref, "labeled", "priority:"+tracker)
	if err != nil {
		return err
	}
	c.who = who
	switch {
	case who == "":
		x.refused, x.reason = &c, "who made the change is unknown, so its right to make it cannot be checked"
		return nil
	case who != x.t.Author:
		allowed, err := s.provider.CanWrite(ctx, x.repo.Repo, who)
		if err != nil {
			return err
		}
		if !allowed {
			x.refused, x.reason = &c, fmt.Sprintf("@%s has no write access to %s", who, x.repo.Repo)
			return nil
		}
	}
	tk, err := s.tickets.SetPriority(ctx, "human:"+who, x.tk.Ref, tracker)
	if err != nil {
		return err
	}
	x.tk = tk
	return nil
}

// size takes the ticket's size from its size label; the refiner keeps the
// label in step with the standard (spec 4.7).
func (s *Syncer) size(ctx context.Context, x *syncing) error {
	size, _ := requested(labelValues(x.t.Labels, "size:", core.Sizes()), x.tk.Size)
	if size == "" {
		return nil
	}
	tk, err := s.tickets.SetSize(ctx, "system", x.tk.Ref, size)
	if err == nil {
		x.tk = tk
	}
	return err
}

// settlePriority makes the tracker show istari's priority once the state is
// reconciled: it reverts a refused request, and writes a priority set in
// istari unless a state write already carried it.
func (s *Syncer) settlePriority(ctx context.Context, x *syncing) error {
	if x.refused != nil {
		return s.revert(ctx, x, *x.refused, x.reason)
	}
	if x.wrote {
		return nil
	}
	var want []string
	if x.tk.Priority != "" {
		want = []string{x.tk.Priority}
	}
	if !slices.Equal(labelValues(x.t.Labels, "priority:", core.Priorities()), want) {
		if err := s.push(ctx, x, x.tk); err != nil {
			return err
		}
		x.report.Pushed++
		return nil
	}
	if x.base.Priority != x.tk.Priority {
		x.base.Priority = x.tk.Priority
		return s.record(ctx, x, EventSyncObserved, "system", x.tk, map[string]any{"priority": x.tk.Priority})
	}
	return nil
}

// writeLabels writes tk's state, hold and priority to the tracker.
func (s *Syncer) writeLabels(ctx context.Context, x *syncing, tk core.Ticket) error {
	if err := s.provider.SetState(ctx, x.t.Ref, State{State: tk.State, Hold: tk.Hold, Priority: tk.Priority}); err != nil {
		return err
	}
	x.wrote = true
	x.base.Priority = tk.Priority
	return nil
}

// push writes istari's state of tk to the tracker.
func (s *Syncer) push(ctx context.Context, x *syncing, tk core.Ticket) error {
	if err := s.writeLabels(ctx, x, tk); err != nil {
		return err
	}
	return s.record(ctx, x, EventSyncPushed, "system", tk, map[string]any{"state": tk.State, "hold": tk.Hold, "priority": tk.Priority})
}

// humanTriggers are the transitions a human may request by editing the
// tracker: the ones spec 4.1 has a human initiate (accept is the manual
// admission of spec 4.5). The others are made by istari itself.
var humanTriggers = []string{"accept", "cancel", "reopen"}

// change is a human's edit in the tracker.
type change struct {
	what string // e.g. "the label `istari:ready`"
	who  string // the login of who made it, "" if unknown
}

// request treats the tracker's differing state as a human's request.
func (s *Syncer) request(ctx context.Context, x *syncing, tracker side) error {
	var target core.TicketState
	var kind, label string
	var c change
	switch local := x.tk.State; {
	case tracker.State != "" && tracker.State != local:
		target, kind, label = tracker.State, "labeled", "istari:"+string(tracker.State)
		c.what = "the label `" + label + "`"
	case tracker.Closed && !finished(local):
		target, kind, c.what = core.TicketCanceled, "closed", "closing the issue"
	case !tracker.Closed && local == core.TicketCanceled:
		target, kind, c.what = core.TicketNew, "reopened", "reopening the issue"
	case !tracker.Closed && local == core.TicketDone:
		target, kind, c.what = core.TicketDone, "reopened", "reopening the issue"
	default:
		target, kind, label, c.what = local, "labeled", "istari:hold:"+tracker.Hold, "the hold label"
	}
	if kind == "closed" {
		merged, err := s.merged(ctx, x.tk.Ref, true)
		if err != nil {
			return err
		}
		if merged {
			return s.finishMerged(ctx, x)
		}
	}
	who, err := s.provider.LastActor(ctx, x.t.Ref, kind, label)
	if err != nil {
		return err
	}
	c.who = who
	switch {
	case target == x.tk.State && x.tk.State == core.TicketDone:
		return s.revert(ctx, x, c, "a done ticket is not reopened; file a new ticket that links this one")
	case target == x.tk.State:
		return s.revert(ctx, x, c, "holds are set by istari; use istari ticket pause, resume or retry")
	}
	trigger, ok := core.TriggerTo(x.tk.State, target)
	if !ok {
		return s.revert(ctx, x, c, fmt.Sprintf("a ticket in `%s` cannot go to `%s`", x.tk.State, target))
	}
	if !slices.Contains(humanTriggers, trigger) {
		return s.revert(ctx, x, c, fmt.Sprintf("`%s` → `%s` is %s, and istari makes this transition itself", x.tk.State, target, trigger))
	}
	if who == "" {
		return s.revert(ctx, x, c, "who made the change is unknown, so its right to make it cannot be checked")
	}
	var conf Conformance
	var more map[string]any
	if trigger == "accept" {
		more = admittedAs{rule: ruleHumanAdmission}.payload()
		// Admission (spec 4.5): a trusted member, and a ticket that meets
		// the standard.
		trusted, err := s.trusted(ctx, x.repo, who)
		if err != nil {
			return err
		}
		if !trusted {
			return s.revert(ctx, x, c, fmt.Sprintf("@%s is not a trusted member of %s, so cannot admit tickets", who, x.repo.Project))
		}
		if conf, err = s.conformance(ctx, x.repo, x.t); err != nil {
			return err
		}
		if len(conf.Deviations) > 0 {
			x.base.Sponsor = who
			return s.revert(ctx, x, c, "the ticket does not meet the standard yet ("+strings.Join(conf.Deviations, "; ")+"); it is refined first and admitted as soon as it does")
		}
		if err := s.tablePriority(ctx, x, conf); err != nil {
			return err
		}
	} else {
		allowed, err := s.provider.CanWrite(ctx, x.repo.Repo, who)
		if err != nil {
			return err
		}
		if !allowed {
			return s.revert(ctx, x, c, fmt.Sprintf("@%s has no write access to %s", who, x.repo.Repo))
		}
	}
	x.base.Hash = conf.Hash
	next, err := s.tickets.FireWith(ctx, "human:"+who, x.tk.Ref, trigger, more)
	if errors.As(err, new(*core.Error)) {
		return s.revert(ctx, x, c, err.Error())
	}
	if err != nil {
		return err
	}
	x.report.Transitioned++
	if err := s.push(ctx, x, next); err != nil {
		return err
	}
	x.report.Pushed++
	return nil
}

// merged tells whether the ticket's pull request is merged (SyncOptions.Merged).
func (s *Syncer) merged(ctx context.Context, ref string, fresh bool) (bool, error) {
	if s.opts.Merged == nil {
		return false, nil
	}
	return s.opts.Merged(ctx, ref, fresh)
}

// repairMerged moves a canceled ticket whose pull request the PR watch saw
// merged to done (T11 from canceled, spec 4.8): an earlier sync read the
// issue that merge closed as a cancel. It reports whether it did; the
// tracker then gets done on the reconcile that follows.
func (s *Syncer) repairMerged(ctx context.Context, tk core.Ticket) (bool, error) {
	if tk.State != core.TicketCanceled {
		return false, nil
	}
	merged, err := s.merged(ctx, tk.Ref, false)
	if err != nil || !merged {
		return false, err
	}
	_, err = s.tickets.Fire(ctx, "system", tk.Ref, "merged")
	return err == nil, err
}

// finishMerged moves a ticket whose pull request a human merged to done
// (T11, spec 4.8): the issue that merge closed is not a cancel.
func (s *Syncer) finishMerged(ctx context.Context, x *syncing) error {
	next, err := s.tickets.Fire(ctx, "system", x.tk.Ref, "merged")
	if err != nil {
		return err
	}
	x.report.Transitioned++
	if err := s.push(ctx, x, next); err != nil {
		return err
	}
	x.report.Pushed++
	return nil
}

// revert restores istari's state in the tracker and says why in a comment.
func (s *Syncer) revert(ctx context.Context, x *syncing, c change, reason string) error {
	if err := s.writeLabels(ctx, x, x.tk); err != nil {
		return err
	}
	by, actor := "", "system"
	if c.who != "" {
		by, actor = " by @"+c.who, "human:"+c.who
	}
	body := fmt.Sprintf("istari undid %s%s: %s. The ticket stays in `%s`.", c.what, by, reason, x.tk.State)
	if _, err := s.provider.Comment(ctx, x.t.Ref, body); err != nil {
		return err
	}
	x.report.Reverted++
	return s.record(ctx, x, EventSyncReverted, actor, x.tk, map[string]any{"change": c.what, "reason": reason})
}

const commentKind = "comment"

// comments delivers each human comment newer than the ticket's first sight
// once, as a sync.comment event; a trusted member's comment on a ticket in
// work also becomes a journal entry, before its event marks it delivered.
func (s *Syncer) comments(ctx context.Context, x *syncing) error {
	ref := x.t.Ref.String()
	if x.t.Comments == 0 || (!x.t.Updated.IsZero() && s.updated[ref].Equal(x.t.Updated)) {
		return nil
	}
	comments, err := s.provider.Comments(ctx, x.t.Ref)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if c.Own || c.ID <= x.base.FirstComment {
			continue
		}
		id := ref + "/" + strconv.FormatInt(c.ID, 10)
		if _, err := s.store.Get(ctx, commentKind, id); err == nil {
			continue // delivered before
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := s.journalComment(ctx, x, c); err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"id": c.ID, "author": c.Author, "body": c.Body, "url": c.URL})
		_, err := s.store.Append(ctx, store.Event{Type: EventSyncComment, At: s.opts.Clock(), Actor: "human:" + c.Author, Project: x.tk.Project, Ticket: ref, Payload: raw},
			store.Change{Kind: commentKind, ID: id, Data: json.RawMessage(`{}`)})
		if errors.Is(err, store.ErrConflict) {
			continue // delivered before
		}
		if err != nil {
			return err
		}
		x.report.Comments++
	}
	s.updated[ref] = x.t.Updated
	return nil
}

// journalComment records the comment in the journal when the ticket is in
// work, planning up to awaiting approval, and its author is trusted: the
// worker receives it in its next brief (spec 4.5).
func (s *Syncer) journalComment(ctx context.Context, x *syncing, c Comment) error {
	inWork := x.tk.State != core.TicketNew && x.tk.State != core.TicketReady && !x.tk.State.Finished()
	if s.opts.Comment == nil || !inWork {
		return nil
	}
	trusted, err := s.trusted(ctx, x.repo, c.Author)
	if err != nil || !trusted {
		return err
	}
	return s.opts.Comment(ctx, x.t.Ref, c)
}

// trusted tells whether login may have tickets admitted without a human and
// admit others': the project's trusted list, or write access without one.
func (s *Syncer) trusted(ctx context.Context, repo SyncRepo, login string) (bool, error) {
	if login == "" {
		return false, nil
	}
	if len(repo.Trusted) > 0 {
		return slices.ContainsFunc(repo.Trusted, func(t string) bool { return strings.EqualFold(t, login) }), nil
	}
	return s.provider.CanWrite(ctx, repo.Repo, login)
}

// conformance checks a ticket against its repository's standard; without
// Standards every ticket conforms.
func (s *Syncer) conformance(ctx context.Context, repo SyncRepo, t Ticket) (Conformance, error) {
	if s.opts.Standards == nil {
		return Conformance{}, nil
	}
	return s.opts.Standards.Check(ctx, repo.Repo, t)
}

// admission admits a new ticket that meets the standard (T2) when a trusted
// member asked to, it inherits an admitted ticket's admission, or its author
// admits it (see authorAdmits), and returns a ready ticket whose required
// sections changed to new (T15).
func (s *Syncer) admission(ctx context.Context, x *syncing) error {
	if s.opts.Standards == nil || x.tk.Hold != "" {
		return nil
	}
	switch x.tk.State {
	case core.TicketNew:
		actor, rule := "system", admittedAs{rule: ruleTrustedAuthor}
		sponsored, err := s.sponsored(ctx, x.repo, x.base)
		if err != nil {
			return err
		}
		switch {
		case sponsored:
			actor, rule = "human:"+x.base.Sponsor, admittedAs{rule: ruleHumanAdmission}
		case x.tk.BudgetOf != "":
			// an agent created it for an admitted ticket: it inherits that admission
			rule = admittedAs{rule: ruleAgent, parent: x.tk.BudgetOf}
		default:
			if admits, err := s.authorAdmits(ctx, x.repo, x.t.Author); err != nil || !admits {
				return err
			}
		}
		conf, err := s.conformance(ctx, x.repo, x.t)
		if err != nil || len(conf.Deviations) > 0 {
			return err
		}
		return s.admit(ctx, x, actor, rule, conf)
	case core.TicketReady:
		conf, err := s.conformance(ctx, x.repo, x.t)
		if err != nil {
			return err
		}
		if err := s.notePriority(ctx, x, conf); err != nil {
			return err
		}
		switch {
		case x.base.Hash == "":
			x.base.Hash = conf.Hash // ready before istari kept a hash: from now on
			return s.record(ctx, x, EventSyncObserved, "system", x.tk, map[string]any{"hash": conf.Hash})
		case conf.Hash != x.base.Hash:
			next, err := s.tickets.Fire(ctx, "system", x.tk.Ref, "edited")
			if err != nil {
				return err
			}
			x.report.Transitioned++
			x.base.Hash = ""
			if err := s.push(ctx, x, next); err != nil {
				return err
			}
			x.report.Pushed++
		}
	case core.TicketDone, core.TicketCanceled:
	default:
		conf, err := s.conformance(ctx, x.repo, x.t)
		if err != nil {
			return err
		}
		return s.notePriority(ctx, x, conf)
	}
	return nil
}

// The rules that admit a ticket, as T2's payload names them (spec 4.5).
const (
	ruleTrustedAuthor  = "trusted_author"
	ruleAgent          = "agent"
	ruleHumanAdmission = "human_admission"
)

// admittedAs is the rule that admits a ticket and, for ruleAgent, the
// admitted ticket the agent created it on.
type admittedAs struct{ rule, parent string }

func (a admittedAs) payload() map[string]any {
	p := map[string]any{"rule": a.rule}
	if a.parent != "" {
		p["parent"] = a.parent
	}
	return p
}

// admit fires T2 by rule and writes the new state out, remembering the hash.
// A ticket without a stored priority takes its table's, set by its author
// (spec 4.5).
func (s *Syncer) admit(ctx context.Context, x *syncing, actor string, rule admittedAs, conf Conformance) error {
	if err := s.tablePriority(ctx, x, conf); err != nil {
		return err
	}
	next, err := s.tickets.FireWith(ctx, actor, x.tk.Ref, "accept", rule.payload())
	if err != nil {
		return err
	}
	x.report.Transitioned++
	x.base.Hash = conf.Hash
	x.base.Sponsor = "" // a request admits once: an edit after it is checked again (T15)
	if err := s.push(ctx, x, next); err != nil {
		return err
	}
	x.report.Pushed++
	return nil
}

// tablePriority makes the table's priority the stored one, set by the
// ticket's author, when the ticket has none yet (spec 4.5).
func (s *Syncer) tablePriority(ctx context.Context, x *syncing, conf Conformance) error {
	if x.tk.Priority != "" || conf.Priority == "" {
		return nil
	}
	tk, err := s.tickets.SetPriority(ctx, "human:"+x.t.Author, x.tk.Ref, conf.Priority)
	if err == nil {
		x.tk = tk
	}
	return err
}

// notePriority says once in the journal that the table's priority differs
// from the stored one, naming both; the stored priority stays (spec 4.5).
func (s *Syncer) notePriority(ctx context.Context, x *syncing, conf Conformance) error {
	if s.opts.Note == nil || conf.Priority == "" || x.tk.Priority == "" || conf.Priority == x.tk.Priority {
		return nil
	}
	pair := x.tk.Priority + "/" + conf.Priority
	if x.base.PriorityNoted == pair {
		return nil
	}
	text := fmt.Sprintf("The ticket's priority is %s, its table says %s. The priority stays %s: the table's is taken only when the ticket is admitted; change the label `priority:` to change it.", x.tk.Priority, conf.Priority, x.tk.Priority)
	if err := s.opts.Note(ctx, x.t.Ref, text); err != nil {
		return err
	}
	x.base.PriorityNoted = pair
	return s.record(ctx, x, EventSyncObserved, "system", x.tk, map[string]any{"priority": x.tk.Priority, "table_priority": conf.Priority})
}

// repoOf returns the synced repository of ref.
func (s *Syncer) repoOf(ref Ref) (SyncRepo, bool) {
	for _, r := range s.repos {
		if strings.EqualFold(r.Repo, ref.Repo) {
			return r, true
		}
	}
	return SyncRepo{}, false
}

// SetPriority sets a ticket's priority as core.Tickets.SetPriority does and,
// for a ticket of a synced repository, writes its label at once.
func (s *Syncer) SetPriority(ctx context.Context, actor, refText, priority string) (core.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, err := s.tickets.SetPriority(ctx, actor, refText, priority)
	if err != nil {
		return core.Ticket{}, err
	}
	ref, err := ParseRef(refText)
	if err != nil {
		return tk, nil // not a tracker reference: nothing to write
	}
	repo, ok := s.repoOf(ref)
	if !ok {
		return tk, nil
	}
	t, err := s.provider.Get(ctx, ref)
	if err != nil {
		return tk, err
	}
	if err := s.reconcile(ctx, repo, t, &SyncReport{}); err != nil {
		return tk, err
	}
	return s.tickets.Get(ctx, refText)
}

// Created mirrors tickets istari just created in the tracker for actor, who
// is the actor of their create transition whatever login the tracker shows as
// their author. With budgetOf, an admitted ticket an agent was working on,
// each is admitted when it meets the standard and shares that ticket's
// budget (spec section 4.5: it inherits the parent's admission and budget).
func (s *Syncer) Created(ctx context.Context, actor string, refs []Ref, budgetOf string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parent core.Ticket
	if budgetOf != "" {
		var err error
		if parent, err = s.tickets.Get(ctx, budgetOf); err != nil {
			return err
		}
	}
	var errs []error
	for _, ref := range refs {
		if err := s.created(ctx, actor, ref, parent); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ref, err))
		}
	}
	return errors.Join(errs...)
}

// created mirrors one ticket istari created; parent is the ticket an agent
// was working on, zero for none. The ticket takes its table's priority at
// once, or the parent's when the table states none (spec 4.5).
func (s *Syncer) created(ctx context.Context, actor string, ref Ref, parent core.Ticket) error {
	repo, ok := s.repoOf(ref)
	if !ok {
		return nil // a repository istari does not sync
	}
	t, err := s.provider.Get(ctx, ref)
	if err != nil {
		return err
	}
	conf, err := s.conformance(ctx, repo, t)
	if err != nil {
		return err
	}
	inherit := parent.Ref != "" && parent.State != core.TicketNew && parent.State != core.TicketCanceled
	nt := core.NewTicket{Ref: ref.String(), Project: repo.Project, Title: t.Title, Author: t.Author, URL: t.URL}
	if inherit {
		nt.BudgetOf = parent.Ref
	}
	tk, err := s.tickets.CreateWith(ctx, actor, nt)
	var ce *core.Error
	switch {
	case errors.As(err, &ce) && ce.Code == core.CodeInvalidTicket:
		// istari has it already
	case err != nil:
		return err
	default:
		own := conf
		if own.Priority == "" {
			own.Priority = parent.Priority
		}
		if err := s.tablePriority(ctx, &syncing{repo: repo, t: t, tk: tk}, own); err != nil {
			return err
		}
	}
	var report SyncReport
	if err := s.reconcile(ctx, repo, t, &report); err != nil {
		return err
	}
	if !inherit {
		return nil
	}
	if tk, err = s.tickets.Get(ctx, ref.String()); err != nil || tk.State != core.TicketNew {
		return err
	}
	if len(conf.Deviations) > 0 {
		return nil // it waits for the refiner like any other
	}
	base, version, err := s.base(ctx, ref.String())
	if err != nil {
		return err
	}
	return s.admit(ctx, &syncing{repo: repo, t: t, tk: tk, base: base, version: version, report: &report}, "system", admittedAs{rule: ruleAgent, parent: parent.Ref}, conf)
}

// Ready admits a ticket by hand (istari ticket ready): login must be a
// trusted member and the ticket must meet the standard.
func (s *Syncer) Ready(ctx context.Context, login, refText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, err := ParseRef(refText)
	if err != nil {
		return &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
	}
	repo, ok := s.repoOf(ref)
	if !ok {
		return &core.Error{Code: core.CodeNotFound, Message: fmt.Sprintf("%s is in no synced repository", ref)}
	}
	t, err := s.provider.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return &core.Error{Code: core.CodeNotFound, Message: fmt.Sprintf("%s does not exist", ref)}
	}
	if err != nil {
		return err
	}
	var report SyncReport
	if err := s.reconcile(ctx, repo, t, &report); err != nil { // mirror it first
		return err
	}
	tk, err := s.tickets.Get(ctx, ref.String())
	if err != nil {
		return err
	}
	if tk.State != core.TicketNew {
		return &core.Error{Code: core.CodeInvalidState, Message: fmt.Sprintf("%s is %s, not new", ref, tk.State)}
	}
	trusted, err := s.trusted(ctx, repo, login)
	if err != nil {
		return err
	}
	if !trusted {
		return &core.Error{Code: CodeNotTrusted, Message: fmt.Sprintf("%s is not a trusted member of %s", login, repo.Project)}
	}
	conf, err := s.conformance(ctx, repo, t)
	if err != nil {
		return err
	}
	base, version, err := s.base(ctx, ref.String())
	if err != nil {
		return err
	}
	x := &syncing{repo: repo, t: t, tk: tk, base: base, version: version, report: &report}
	if len(conf.Deviations) > 0 {
		// Refined first, then admitted by login (spec 4.5).
		x.base.Sponsor = login
		return s.record(ctx, x, EventAdmissionRequested, "human:"+login, tk, map[string]any{"deviations": conf.Deviations})
	}
	return s.admit(ctx, x, "human:"+login, admittedAs{rule: ruleHumanAdmission}, conf)
}
