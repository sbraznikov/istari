package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sbraznikov/istari/internal/store"
)

// TicketState is a state of the ticket machine (spec section 4.1).
type TicketState string

// Ticket states.
const (
	TicketNew              TicketState = "new"
	TicketReady            TicketState = "ready"
	TicketPlanning         TicketState = "planning"
	TicketInProgress       TicketState = "in_progress"
	TicketInReview         TicketState = "in_review"
	TicketAwaitingApproval TicketState = "awaiting_approval"
	TicketDone             TicketState = "done"
	TicketCanceled         TicketState = "canceled"
)

// States are the ticket states, in the order of the pipeline.
func States() []TicketState {
	return []TicketState{TicketNew, TicketReady, TicketPlanning, TicketInProgress, TicketInReview, TicketAwaitingApproval, TicketDone, TicketCanceled}
}

// Finished reports whether the state is final, done or canceled: the
// ticket has no open tasks (spec section 4.1).
func (s TicketState) Finished() bool { return s == TicketDone || s == TicketCanceled }

// HoldReasons are the reasons a ticket can be held (spec section 4.1).
func HoldReasons() []string {
	return []string{"needs_input", "attempts_exhausted", "budget_exceeded", "paused", "waiting_dependency", "waiting_zone"}
}

// Event types of the ticket machine.
const (
	EventTicketTransitioned = "ticket.transitioned"
	EventTicketHoldSet      = "ticket.hold_set"
	EventTicketHoldCleared  = "ticket.hold_cleared"
	EventTicketPrioritized  = "ticket.prioritized"
	EventTicketSized        = "ticket.sized"
	EventTicketNeedsSet     = "ticket.needs_set"
	EventTicketChildrenSet  = "ticket.children_set"
)

// Sizes are the T-shirt sizes of a ticket, smallest first (spec section 4.7).
func Sizes() []string { return []string{"XS", "S", "M", "L", "XL"} }

// Priorities are the ticket priorities, most urgent first (spec section 4.9).
func Priorities() []string { return []string{"P0", "P1", "P2", "P3"} }

// DefaultPriority is the priority of a ticket nobody gave one (spec 4.9).
const DefaultPriority = "P2"

const ticketKind = "ticket"

// Ticket is the projection of one ticket.
type Ticket struct {
	Ref     string      `json:"ref"` // e.g. gh:acme/web#123
	Project string      `json:"project,omitempty"`
	Title   string      `json:"title,omitempty"`
	Author  string      `json:"author,omitempty"` // the tracker login of who created it
	URL     string      `json:"url,omitempty"`
	State   TicketState `json:"state"`
	Hold    string      `json:"hold,omitempty"` // a hold reason, or empty
	// Priority is P0…P3, set by a human; empty means the default.
	Priority string `json:"priority,omitempty"`
	Size     string `json:"size,omitempty"` // XS…XL, or empty
	// Needs are the tickets this one waits for (spec section 4.9), as its
	// text names them.
	Needs []string `json:"needs,omitempty"`
	// BudgetOf is the ticket whose budget this one shares: it was created by
	// an agent at work on that ticket (spec section 4.5).
	BudgetOf string `json:"budget_of,omitempty"`
	// Children are the tickets a split made of this one (T5); it is done
	// when they all are (T12).
	Children []string  `json:"children,omitempty"`
	Updated  time.Time `json:"updated"`

	version int64
}

// transition is one row of the table in spec section 4.1.
type transition struct {
	id      string
	from    TicketState
	trigger string
	to      TicketState
}

// transitions is the table of spec section 4.1. T1 (create) is Tickets.Create.
// No other transition exists.
var transitions = func() []transition {
	rows := []transition{
		{"T2", TicketNew, "accept", TicketReady},
		{"T3", TicketReady, "start", TicketPlanning},
		{"T4", TicketPlanning, "plan_accepted", TicketInProgress},
		{"T5", TicketPlanning, "split", TicketInProgress},
		{"T6", TicketInProgress, "pr_ready", TicketInReview},
		{"T7", TicketInReview, "changes_requested", TicketInProgress},
		{"T8", TicketInReview, "review_passed", TicketAwaitingApproval},
		{"T9", TicketAwaitingApproval, "changes_requested", TicketInProgress},
		{"T10", TicketAwaitingApproval, "merge_failed", TicketInProgress},
		{"T11", TicketAwaitingApproval, "merged", TicketDone},
		{"T12", TicketInProgress, "children_done", TicketDone},
		{"T14", TicketCanceled, "reopen", TicketNew},
		{"T15", TicketReady, "edited", TicketNew},
	}
	// T13: any state except done and canceled → canceled.
	for _, from := range []TicketState{TicketNew, TicketReady, TicketPlanning, TicketInProgress, TicketInReview, TicketAwaitingApproval} {
		rows = append(rows, transition{"T13", from, "cancel", TicketCanceled})
	}
	return rows
}()

// TriggerTo returns the trigger of the transition from one state to another,
// e.g. accept for new → ready. Where two rows connect the same states (T9 and
// T10), the first row of the table is taken.
func TriggerTo(from, to TicketState) (string, bool) {
	for _, row := range transitions {
		if row.from == from && row.to == to {
			return row.trigger, true
		}
	}
	return "", false
}

func findTransition(from TicketState, trigger string) (transition, bool) {
	for _, row := range transitions {
		if row.from == from && row.trigger == trigger {
			return row, true
		}
	}
	return transition{}, false
}

// Guard is the condition of a transition. It returns nil to allow the
// transition, or an error that explains the refusal.
type Guard func(ctx context.Context, t Ticket) error

// Guards maps a transition id ("T2" … "T15") to its condition. A transition
// without a guard is allowed whenever the table allows it. The conditions
// themselves (admission, free slots, plan, CI…) belong to later components.
type Guards map[string]Guard

// TicketOptions configures Tickets.
type TicketOptions struct {
	Clock  func() time.Time
	Guards Guards
}

// Tickets runs the ticket state machine on top of a store. Every change is one
// store.Append, conditional on the version that was read.
type Tickets struct {
	store store.Store
	opts  TicketOptions
}

// NewTickets returns the ticket machine.
func NewTickets(s store.Store, opts TicketOptions) *Tickets {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	clock := opts.Clock
	opts.Clock = func() time.Time { return clock().UTC() }
	for id := range opts.Guards {
		if !slices.ContainsFunc(transitions, func(row transition) bool { return row.id == id }) {
			panic(fmt.Sprintf("core: guard for unknown transition %q", id))
		}
	}
	return &Tickets{store: s, opts: opts}
}

// Get returns one ticket.
func (m *Tickets) Get(ctx context.Context, ref string) (Ticket, error) {
	rec, err := m.store.Get(ctx, ticketKind, ref)
	if errors.Is(err, store.ErrNotFound) {
		return Ticket{}, errorf(CodeNotFound, "ticket %s does not exist", ref)
	}
	if err != nil {
		return Ticket{}, err
	}
	return decodeTicket(rec)
}

// List returns all tickets ordered by ref.
func (m *Tickets) List(ctx context.Context) ([]Ticket, error) {
	recs, err := m.store.List(ctx, ticketKind)
	if err != nil {
		return nil, err
	}
	tickets := make([]Ticket, 0, len(recs))
	for _, rec := range recs {
		ticket, err := decodeTicket(rec)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, ticket)
	}
	return tickets, nil
}

// NewTicket describes a ticket to create, as the tracker has it.
type NewTicket struct {
	Ref, Project, Title, Author, URL string
	BudgetOf                         string // the admitted ticket whose budget it shares, if any
}

// Create is transition T1: a new ticket in state new.
func (m *Tickets) Create(ctx context.Context, actor, ref, project string) (Ticket, error) {
	return m.CreateWith(ctx, actor, NewTicket{Ref: ref, Project: project})
}

// CreateWith is Create with what the tracker knows about the ticket.
func (m *Tickets) CreateWith(ctx context.Context, actor string, nt NewTicket) (Ticket, error) {
	ref := nt.Ref
	if ref == "" {
		return Ticket{}, errorf(CodeInvalidTicket, "a ticket needs a reference")
	}
	ticket := Ticket{Ref: ref, Project: nt.Project, Title: nt.Title, Author: nt.Author, URL: nt.URL, BudgetOf: nt.BudgetOf, State: TicketNew, Updated: m.opts.Clock()}
	payload := transitionPayload("", TicketNew, "create", actor)
	if nt.BudgetOf != "" {
		payload["budget_of"] = nt.BudgetOf
	}
	err := m.commit(ctx, actor, EventTicketTransitioned, ticket, payload)
	if errors.Is(err, store.ErrConflict) {
		return Ticket{}, errorf(CodeInvalidTicket, "ticket %s already exists", ref)
	}
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// Recover creates a ticket istari lost, in the state and hold its tracker
// labels have (spec section 4.6: istari recover rebuilds the database from
// the tracker). It is the history's start, like create.
func (m *Tickets) Recover(ctx context.Context, actor string, nt NewTicket, state TicketState, hold string) (Ticket, error) {
	if nt.Ref == "" {
		return Ticket{}, errorf(CodeInvalidTicket, "a ticket needs a reference")
	}
	if !slices.Contains(States(), state) {
		return Ticket{}, errorf(CodeInvalidTicket, "%q is not a ticket state", state)
	}
	if hold != "" && !slices.Contains(HoldReasons(), hold) {
		return Ticket{}, errorf(CodeInvalidTicket, "%q is not a hold reason", hold)
	}
	ticket := Ticket{Ref: nt.Ref, Project: nt.Project, Title: nt.Title, Author: nt.Author, URL: nt.URL, BudgetOf: nt.BudgetOf, State: state, Hold: hold, Updated: m.opts.Clock()}
	payload := transitionPayload("", state, triggerRecover, actor)
	payload["reason"] = hold
	if nt.BudgetOf != "" {
		payload["budget_of"] = nt.BudgetOf
	}
	err := m.commit(ctx, actor, EventTicketTransitioned, ticket, payload)
	if errors.Is(err, store.ErrConflict) {
		return Ticket{}, errorf(CodeInvalidTicket, "ticket %s already exists", nt.Ref)
	}
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

const triggerRecover = "recover"

// Fire applies the transition for trigger from the ticket's current state.
// A pair (state, trigger) outside the table fails with invalid_transition and
// a refused guard with guard_failed; neither appends anything. While a hold is
// set the state does not change: only cancel is allowed.
func (m *Tickets) Fire(ctx context.Context, actor, ref, trigger string) (Ticket, error) {
	var ticket Ticket
	err := retry(func() error {
		var err error
		if ticket, err = m.Get(ctx, ref); err != nil {
			return err
		}
		row, ok := findTransition(ticket.State, trigger)
		if !ok {
			return errorf(CodeInvalidTransition, "ticket %s is %s; %s is not allowed from there", ref, ticket.State, trigger)
		}
		if ticket.Hold != "" && trigger != "cancel" {
			return errorf(CodeTicketHeld, "ticket %s is on hold (%s)", ref, ticket.Hold)
		}
		if guard := m.opts.Guards[row.id]; guard != nil {
			if err := guard(ctx, ticket); err != nil {
				return &Error{Code: CodeGuardFailed, Message: fmt.Sprintf("%s %s: %v", row.id, trigger, err), Err: err}
			}
		}
		payload := transitionPayload(row.from, row.to, trigger, actor)
		ticket.State = row.to
		if row.to == TicketCanceled && ticket.Hold != "" {
			// A finished ticket carries no hold; cancel ends it.
			payload["hold_cleared"] = ticket.Hold
			ticket.Hold = ""
		}
		ticket.Updated = m.opts.Clock()
		var ended []store.Change
		if ticket.State.Finished() {
			// A finished ticket has no open tasks (spec 4.1).
			var ids []string
			if ids, ended, err = cancelTasksOf(ctx, m.store, ref); err != nil {
				return err
			}
			if len(ids) > 0 {
				payload["tasks_canceled"] = ids
			}
		}
		return m.commit(ctx, actor, EventTicketTransitioned, ticket, payload, ended...)
	})
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// SetHold sets the hold flag. The state does not change. A hold can be set on
// any unfinished state, and only when no hold is set.
func (m *Tickets) SetHold(ctx context.Context, actor, ref, reason string) (Ticket, error) {
	if !slices.Contains(HoldReasons(), reason) {
		return Ticket{}, errorf(CodeInvalidTicket, "hold reason %q is not one of %s", reason, strings.Join(HoldReasons(), ", "))
	}
	return m.changeHold(ctx, actor, ref, EventTicketHoldSet, func(ticket *Ticket) (string, error) {
		if ticket.State == TicketDone || ticket.State == TicketCanceled {
			return "", errorf(CodeInvalidState, "ticket %s is %s; a finished ticket cannot be held", ref, ticket.State)
		}
		if ticket.Hold != "" {
			return "", errorf(CodeTicketHeld, "ticket %s is already on hold (%s)", ref, ticket.Hold)
		}
		ticket.Hold = reason
		return reason, nil
	})
}

// ClearHold clears the hold flag; the ticket continues from the same state.
// A non-empty reason clears only a hold of that reason: resume lifts paused,
// retry lifts attempts_exhausted (spec section 4.1).
func (m *Tickets) ClearHold(ctx context.Context, actor, ref, reason string) (Ticket, error) {
	return m.changeHold(ctx, actor, ref, EventTicketHoldCleared, func(ticket *Ticket) (string, error) {
		if ticket.Hold == "" {
			return "", errorf(CodeInvalidState, "ticket %s is not on hold", ref)
		}
		if reason != "" && ticket.Hold != reason {
			return "", errorf(CodeInvalidState, "ticket %s is on hold for %s, not %s", ref, ticket.Hold, reason)
		}
		reason := ticket.Hold
		ticket.Hold = ""
		return reason, nil
	})
}

// changeHold re-reads the ticket, lets mutate set or clear the hold and
// return the hold reason the event records, and commits one event of type typ.
func (m *Tickets) changeHold(ctx context.Context, actor, ref, typ string, mutate func(*Ticket) (string, error)) (Ticket, error) {
	var ticket Ticket
	err := retry(func() error {
		var err error
		if ticket, err = m.Get(ctx, ref); err != nil {
			return err
		}
		reason, err := mutate(&ticket)
		if err != nil {
			return err
		}
		ticket.Updated = m.opts.Clock()
		return m.commit(ctx, actor, typ, ticket, map[string]any{"reason": reason, "state": ticket.State, "actor": actor})
	})
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// SetPriority sets the ticket's priority. Only a human sets it (spec 4.9):
// an actor other than human:<login> is refused with human_only. Setting the
// priority a ticket already has records nothing.
func (m *Tickets) SetPriority(ctx context.Context, actor, ref, priority string) (Ticket, error) {
	if !strings.HasPrefix(actor, "human:") {
		return Ticket{}, errorf(CodeHumanOnly, "only a human sets the priority of %s, not %s", ref, actor)
	}
	if !slices.Contains(Priorities(), priority) {
		return Ticket{}, errorf(CodeInvalidTicket, "priority %q is not one of %s", priority, strings.Join(Priorities(), ", "))
	}
	var ticket Ticket
	err := retry(func() error {
		var err error
		if ticket, err = m.Get(ctx, ref); err != nil {
			return err
		}
		from := ticket.Priority
		if from == priority {
			return nil // already set: nothing to record
		}
		ticket.Priority = priority
		ticket.Updated = m.opts.Clock()
		return m.commit(ctx, actor, EventTicketPrioritized, ticket, map[string]any{"from": from, "priority": priority, "actor": actor})
	})
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// SetSize sets the ticket's size. The refiner, the planner and humans may
// change it (spec 4.7); every change is an event.
func (m *Tickets) SetSize(ctx context.Context, actor, ref, size string) (Ticket, error) {
	if !slices.Contains(Sizes(), size) {
		return Ticket{}, errorf(CodeInvalidTicket, "size %q is not one of %s", size, strings.Join(Sizes(), ", "))
	}
	var ticket Ticket
	err := retry(func() error {
		var err error
		if ticket, err = m.Get(ctx, ref); err != nil {
			return err
		}
		from := ticket.Size
		ticket.Size = size
		ticket.Updated = m.opts.Clock()
		return m.commit(ctx, actor, EventTicketSized, ticket, map[string]any{"from": from, "size": size, "actor": actor})
	})
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// SetNeeds records the tickets this one waits for. The same needs record
// nothing.
func (m *Tickets) SetNeeds(ctx context.Context, actor, ref string, needs []string) (Ticket, error) {
	needs = slices.Clone(needs)
	slices.Sort(needs)
	needs = slices.Compact(needs)
	var ticket Ticket
	err := retry(func() error {
		var err error
		if ticket, err = m.Get(ctx, ref); err != nil {
			return err
		}
		if slices.Equal(ticket.Needs, needs) {
			return nil
		}
		if slices.Contains(needs, ref) {
			return errorf(CodeInvalidTicket, "ticket %s cannot need itself", ref)
		}
		ticket.Needs = needs
		ticket.Updated = m.opts.Clock()
		return m.commit(ctx, actor, EventTicketNeedsSet, ticket, map[string]any{"needs": needs, "actor": actor})
	})
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// SetChildren records the tickets a split made of this one.
func (m *Tickets) SetChildren(ctx context.Context, actor, ref string, children []string) (Ticket, error) {
	var ticket Ticket
	err := retry(func() error {
		var err error
		if ticket, err = m.Get(ctx, ref); err != nil {
			return err
		}
		ticket.Children = slices.Clone(children)
		ticket.Updated = m.opts.Clock()
		return m.commit(ctx, actor, EventTicketChildrenSet, ticket, map[string]any{"children": children, "actor": actor})
	})
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

func transitionPayload(from, to TicketState, trigger, actor string) map[string]any {
	return map[string]any{"from": from, "to": to, "trigger": trigger, "actor": actor}
}

func (m *Tickets) commit(ctx context.Context, actor, typ string, ticket Ticket, payload map[string]any, more ...store.Change) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data, err := json.Marshal(ticket)
	if err != nil {
		return err
	}
	_, err = m.store.Append(ctx, store.Event{
		Type: typ, At: ticket.Updated, Actor: actor, Project: ticket.Project, Ticket: ticket.Ref, Payload: raw,
	}, append([]store.Change{{Kind: ticketKind, ID: ticket.Ref, Data: data, IfVersion: ticket.version}}, more...)...)
	return err
}

func decodeTicket(rec store.Record) (Ticket, error) {
	var ticket Ticket
	if err := json.Unmarshal(rec.Data, &ticket); err != nil {
		return Ticket{}, fmt.Errorf("core: ticket %s: %w", rec.ID, err)
	}
	ticket.version = rec.Version
	return ticket, nil
}

// ReplayTicket rebuilds a ticket from its events in log order, using nothing
// but the log. It fails unless the events are the valid history of one ticket:
// they start with create, every transition is a row of the table from the
// state the ticket was in, and holds are set and cleared only on a ticket.
func ReplayTicket(events []store.Event) (Ticket, error) {
	var ticket Ticket
	invalid := func(e store.Event, format string, args ...any) error {
		return fmt.Errorf("core: replay %s at %s: %s", e.Ticket, e.Cursor, fmt.Sprintf(format, args...))
	}
	for _, e := range events {
		switch e.Type {
		case EventTicketTransitioned, EventTicketHoldSet, EventTicketHoldCleared, EventTicketPrioritized, EventTicketSized, EventTicketNeedsSet, EventTicketChildrenSet:
		default:
			continue
		}
		var p struct {
			From     TicketState `json:"from"`
			To       TicketState `json:"to"`
			Trigger  string      `json:"trigger"`
			Reason   string      `json:"reason"`
			Priority string      `json:"priority"`
			Size     string      `json:"size"`
			Needs    []string    `json:"needs"`
			Children []string    `json:"children"`
			BudgetOf string      `json:"budget_of"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return Ticket{}, invalid(e, "%v", err)
		}
		if ticket.Ref == "" {
			switch {
			case e.Type == EventTicketTransitioned && p.Trigger == "create":
				ticket = Ticket{Ref: e.Ticket, Project: e.Project, State: TicketNew, BudgetOf: p.BudgetOf, Updated: e.At}
			case e.Type == EventTicketTransitioned && p.Trigger == triggerRecover:
				ticket = Ticket{Ref: e.Ticket, Project: e.Project, State: p.To, Hold: p.Reason, BudgetOf: p.BudgetOf, Updated: e.At}
			default:
				return Ticket{}, invalid(e, "the history does not start with create or recover")
			}
			continue
		}
		if e.Ticket != ticket.Ref {
			return Ticket{}, invalid(e, "event of another ticket than %s", ticket.Ref)
		}
		switch e.Type {
		case EventTicketTransitioned:
			row, ok := findTransition(ticket.State, p.Trigger)
			if !ok || p.From != ticket.State || p.To != row.to {
				return Ticket{}, invalid(e, "%s → %s by %s is not a transition from %s", p.From, p.To, p.Trigger, ticket.State)
			}
			ticket.State = row.to
			if row.to == TicketCanceled {
				ticket.Hold = ""
			}
		case EventTicketHoldSet:
			ticket.Hold = p.Reason
		case EventTicketHoldCleared:
			ticket.Hold = ""
		case EventTicketPrioritized:
			ticket.Priority = p.Priority
		case EventTicketSized:
			ticket.Size = p.Size
		case EventTicketNeedsSet:
			ticket.Needs = p.Needs
		case EventTicketChildrenSet:
			ticket.Children = p.Children
		}
		ticket.Updated = e.At
	}
	if ticket.Ref == "" {
		return Ticket{}, errorf(CodeNotFound, "no ticket events")
	}
	return ticket, nil
}
