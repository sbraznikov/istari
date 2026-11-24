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
}

// Standards checks tickets against their repository's ticket standard.
type Standards interface {
	Check(ctx context.Context, repo string, t Ticket) (Conformance, error)
}

// Conformance is the result of a structural check.
type Conformance struct {
	Deviations []string // empty: the ticket meets the standard
	Hash       string   // digest of the required sections, for T15
}

// SyncOptions configures a Syncer.
type SyncOptions struct {
	Clock func() time.Time
	// Standards turns on admission (T2) and the re-check on edit (T15);
	// without it the sync only mirrors states.
	Standards Standards
}

// Codes of admission refusals.
const (
	CodeNotTrusted  = "not_trusted"
	CodeNotStandard = "not_standard"
)

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
		if base.side.Closed && finished(tk.State) {
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

// baseRecord is what the sync remembers per ticket.
type baseRecord struct {
	side
	// FirstComment is the id of the last comment that existed when istari
	// first saw the ticket; earlier comments are not delivered as events.
	FirstComment int64 `json:"first_comment,omitempty"`
	// Hash is the digest of the required sections when the ticket was
	// admitted; an edit that changes it in ready is T15.
	Hash string `json:"hash,omitempty"`
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
	if err := s.reconcileState(ctx, x); err != nil {
		return err
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
	case tracker == x.base.side || tracker.State == "" || tracker == local:
		// The tracker still has what istari last wrote, or no state yet, or
		// only stray extra labels: istari's state goes out.
		if err := s.push(ctx, x, x.tk); err != nil {
			return err
		}
		x.report.Pushed++
		return nil
	default:
		return s.request(ctx, x, tracker)
	}
}

// push writes istari's state of tk to the tracker.
func (s *Syncer) push(ctx context.Context, x *syncing, tk core.Ticket) error {
	if err := s.provider.SetState(ctx, x.t.Ref, State{State: tk.State, Hold: tk.Hold}); err != nil {
		return err
	}
	return s.record(ctx, x, EventSyncPushed, "system", tk, map[string]any{"state": tk.State, "hold": tk.Hold})
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
	if trigger == "accept" {
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
			return s.revert(ctx, x, c, "the ticket does not meet the standard yet: "+strings.Join(conf.Deviations, "; "))
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
	next, err := s.tickets.Fire(ctx, "human:"+who, x.tk.Ref, trigger)
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

// revert restores istari's state in the tracker and says why in a comment.
func (s *Syncer) revert(ctx context.Context, x *syncing, c change, reason string) error {
	if err := s.provider.SetState(ctx, x.t.Ref, State{State: x.tk.State, Hold: x.tk.Hold}); err != nil {
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
// once, as a sync.comment event.
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
		raw, _ := json.Marshal(map[string]any{"id": c.ID, "author": c.Author, "body": c.Body, "url": c.URL})
		_, err := s.store.Append(ctx, store.Event{Type: EventSyncComment, At: s.opts.Clock(), Actor: "human:" + c.Author, Project: x.tk.Project, Ticket: ref, Payload: raw},
			store.Change{Kind: commentKind, ID: ref + "/" + strconv.FormatInt(c.ID, 10), Data: json.RawMessage(`{}`)})
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

// admission admits a new ticket of a trusted author that meets the standard
// (T2 by the system), and returns a ready ticket whose required sections
// changed to new (T15).
func (s *Syncer) admission(ctx context.Context, x *syncing) error {
	if s.opts.Standards == nil || x.tk.Hold != "" {
		return nil
	}
	switch x.tk.State {
	case core.TicketNew:
		trusted, err := s.trusted(ctx, x.repo, x.t.Author)
		if err != nil || !trusted {
			return err
		}
		conf, err := s.conformance(ctx, x.repo, x.t)
		if err != nil || len(conf.Deviations) > 0 {
			return err
		}
		return s.admit(ctx, x, "system", conf.Hash)
	case core.TicketReady:
		conf, err := s.conformance(ctx, x.repo, x.t)
		if err != nil {
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
	}
	return nil
}

// admit fires T2 and writes the new state out, remembering the hash.
func (s *Syncer) admit(ctx context.Context, x *syncing, actor, hash string) error {
	next, err := s.tickets.Fire(ctx, actor, x.tk.Ref, "accept")
	if err != nil {
		return err
	}
	x.report.Transitioned++
	x.base.Hash = hash
	if err := s.push(ctx, x, next); err != nil {
		return err
	}
	x.report.Pushed++
	return nil
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
	var repo SyncRepo
	for _, r := range s.repos {
		if strings.EqualFold(r.Repo, ref.Repo) {
			repo = r
		}
	}
	if repo.Repo == "" {
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
	if len(conf.Deviations) > 0 {
		return &core.Error{Code: CodeNotStandard, Message: fmt.Sprintf("%s does not meet the standard: %s", ref, strings.Join(conf.Deviations, "; "))}
	}
	base, version, err := s.base(ctx, ref.String())
	if err != nil {
		return err
	}
	x := &syncing{repo: repo, t: t, tk: tk, base: base, version: version, report: &report}
	return s.admit(ctx, x, "human:"+login, conf.Hash)
}
