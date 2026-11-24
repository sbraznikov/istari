package api

import (
	"context"
	"encoding/json"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
)

// EventPlanCreated records the tickets a plan created, by key.
const EventPlanCreated = "ticket.plan_created"

// withoutKey drops the request's idempotency key from ctx: the writes of a
// plan are many, and only its last one records the request.
func withoutKey(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestKeyCtx{}, struct{}{})
}

// createPlan creates a plan's tickets in the tracker, mirrors them into
// istari, and records the refs by key in one event, which also records the
// request: a repeat with the same key answers from it. A ticket an agent
// creates while at work on an admitted ticket inherits its admission and
// budget (spec section 4.5).
func (s *Server) createPlan(ctx context.Context, r TicketPlanRequest) (map[string]string, error) {
	if s.syncer == nil || s.tracker == nil {
		return nil, &core.Error{Code: CodeSyncDisabled, Message: "no project has a ticket tracker"}
	}
	refs, err := tickets.CreatePlan(withoutKey(ctx), s.tracker, r.Plan, s.firstRepo)
	if err != nil {
		return nil, &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
	}
	var budgetOf string
	if run, isAgent := runOf(ctx); isAgent {
		budgetOf = run.Ticket
	}
	out := map[string]string{}
	var list []tickets.Ref
	for _, pt := range r.Plan.Tickets { // in the plan's order
		out[pt.Key] = refs[pt.Key].String()
		list = append(list, refs[pt.Key])
	}
	if err := s.syncer.Created(withoutKey(ctx), list, budgetOf); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(map[string]any{"refs": out, "budget_of": budgetOf})
	if _, err := s.store.Append(ctx, store.Event{Type: EventPlanCreated, At: s.clock().UTC(), Actor: r.Actor, Ticket: list[0].String(), Payload: raw}); err != nil {
		return nil, err
	}
	return out, nil
}

// planAgain answers a repeated plan from the event of its first time.
func (s *Server) planAgain(ctx context.Context, done requestRecord) (any, error) {
	events, err := s.store.Read(ctx, "", store.Filter{Ticket: done.Ticket, TypePrefix: EventPlanCreated}, 0)
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		var p struct {
			Refs map[string]string `json:"refs"`
		}
		if json.Unmarshal(events[i].Payload, &p) == nil && len(p.Refs) > 0 {
			return p.Refs, nil
		}
	}
	return nil, &core.Error{Code: core.CodeNotFound, Message: "the plan of this key is not recorded"}
}
