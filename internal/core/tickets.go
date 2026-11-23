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

// HoldReasons are the reasons a ticket can be held (spec section 4.1).
func HoldReasons() []string {
	return []string{"needs_input", "attempts_exhausted", "budget_exceeded", "paused", "waiting_dependency", "waiting_zone"}
}

// Event types of the ticket machine.
const (
	EventTicketTransitioned = "ticket.transitioned"
	EventTicketHoldSet      = "ticket.hold_set"
	EventTicketHoldCleared  = "ticket.hold_cleared"
)

const ticketKind = "ticket"

// Ticket is the projection of one ticket.
type Ticket struct {
	Ref     string      `json:"ref"` // e.g. gh:acme/web#123
	Project string      `json:"project,omitempty"`
	State   TicketState `json:"state"`
	Hold    string      `json:"hold,omitempty"` // a hold reason, or empty
	Updated time.Time   `json:"updated"`

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

// Create is transition T1: a new ticket in state new.
func (m *Tickets) Create(ctx context.Context, actor, ref, project string) (Ticket, error) {
	if ref == "" {
		return Ticket{}, errorf(CodeInvalidTicket, "a ticket needs a reference")
	}
	ticket := Ticket{Ref: ref, Project: project, State: TicketNew, Updated: m.opts.Clock()}
	err := m.commit(ctx, actor, EventTicketTransitioned, ticket, transitionPayload("", TicketNew, "create", actor))
	if errors.Is(err, store.ErrConflict) {
		return Ticket{}, errorf(CodeInvalidTicket, "ticket %s already exists", ref)
	}
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

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
				return errorf(CodeGuardFailed, "%s %s: %v", row.id, trigger, err)
			}
		}
		ticket.State = row.to
		ticket.Updated = m.opts.Clock()
		return m.commit(ctx, actor, EventTicketTransitioned, ticket, transitionPayload(row.from, row.to, trigger, actor))
	})
	if err != nil {
		return Ticket{}, err
	}
	ticket.version++
	return ticket, nil
}

// SetHold sets the hold flag. The state does not change. A hold can be set on
// any unfinished state, and only when no hold is set.
func (m *Tickets) SetHold(ctx context.Context, actor, ref, reason string) (Ticket, error) {
	if !slices.Contains(HoldReasons(), reason) {
		return Ticket{}, errorf(CodeInvalidTicket, "hold reason %q is not one of %s", reason, strings.Join(HoldReasons(), ", "))
	}
	return m.changeHold(ctx, actor, ref, EventTicketHoldSet, func(ticket *Ticket) error {
		if ticket.State == TicketDone || ticket.State == TicketCanceled {
			return errorf(CodeInvalidState, "ticket %s is %s; a finished ticket cannot be held", ref, ticket.State)
		}
		if ticket.Hold != "" {
			return errorf(CodeTicketHeld, "ticket %s is already on hold (%s)", ref, ticket.Hold)
		}
		ticket.Hold = reason
		return nil
	})
}

// ClearHold clears the hold flag; the ticket continues from the same state.
func (m *Tickets) ClearHold(ctx context.Context, actor, ref string) (Ticket, error) {
	return m.changeHold(ctx, actor, ref, EventTicketHoldCleared, func(ticket *Ticket) error {
		if ticket.Hold == "" {
			return errorf(CodeInvalidState, "ticket %s is not on hold", ref)
		}
		ticket.Hold = ""
		return nil
	})
}

func (m *Tickets) changeHold(ctx context.Context, actor, ref, typ string, mutate func(*Ticket) error) (Ticket, error) {
	var ticket Ticket
	err := retry(func() error {
		var err error
		if ticket, err = m.Get(ctx, ref); err != nil {
			return err
		}
		reason := ticket.Hold
		if err := mutate(&ticket); err != nil {
			return err
		}
		if ticket.Hold != "" {
			reason = ticket.Hold
		}
		ticket.Updated = m.opts.Clock()
		return m.commit(ctx, actor, typ, ticket, map[string]any{"reason": reason, "state": ticket.State, "actor": actor})
	})
	if err != nil {
		return Ticket{}, err
	}
	ticket.version++
	return ticket, nil
}

func transitionPayload(from, to TicketState, trigger, actor string) map[string]any {
	return map[string]any{"from": from, "to": to, "trigger": trigger, "actor": actor}
}

func (m *Tickets) commit(ctx context.Context, actor, typ string, ticket Ticket, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data, err := json.Marshal(ticket)
	if err != nil {
		return err
	}
	_, err = m.store.Append(ctx, store.Event{
		Type: typ, At: m.opts.Clock(), Actor: actor, Project: ticket.Project, Ticket: ticket.Ref, Payload: raw,
	}, store.Change{Kind: ticketKind, ID: ticket.Ref, Data: data, IfVersion: ticket.version})
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
// but the log. It fails if the events do not form a valid history.
func ReplayTicket(events []store.Event) (Ticket, error) {
	var ticket Ticket
	for _, e := range events {
		var p struct {
			From    TicketState `json:"from"`
			To      TicketState `json:"to"`
			Trigger string      `json:"trigger"`
			Reason  string      `json:"reason"`
		}
		switch e.Type {
		case EventTicketTransitioned, EventTicketHoldSet, EventTicketHoldCleared:
		default:
			continue
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return Ticket{}, fmt.Errorf("core: replay %s at %s: %w", e.Ticket, e.Cursor, err)
		}
		if ticket.Ref == "" {
			ticket.Ref, ticket.Project = e.Ticket, e.Project
		}
		switch e.Type {
		case EventTicketTransitioned:
			if p.From != ticket.State {
				return Ticket{}, fmt.Errorf("core: replay %s at %s: transition from %q, but the ticket is %q", e.Ticket, e.Cursor, p.From, ticket.State)
			}
			ticket.State = p.To
		case EventTicketHoldSet:
			ticket.Hold = p.Reason
		case EventTicketHoldCleared:
			ticket.Hold = ""
		}
		ticket.Updated = e.At
	}
	if ticket.Ref == "" {
		return Ticket{}, errorf(CodeNotFound, "no ticket events")
	}
	return ticket, nil
}
