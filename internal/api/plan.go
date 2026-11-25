package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
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
	repo := r.Plan.Repo
	if repo == "" {
		repo = s.firstRepo
	}
	if !s.repos[strings.ToLower(repo)] {
		return nil, &core.Error{Code: core.CodeInvalidTicket, Message: repo + " is no repository istari syncs: add it with istari repo add"}
	}
	r.Plan.Repo = repo
	refs, err := tickets.CreatePlan(withoutKey(ctx), s.tracker, r.Plan, repo)
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
	// Recorded as soon as the tickets exist: a repeat with the same key must
	// not create them again, whatever happens next.
	raw, _ := json.Marshal(map[string]any{"refs": out, "budget_of": budgetOf})
	if _, err := s.store.Append(ctx, store.Event{Type: EventPlanCreated, At: s.clock().UTC(), Actor: r.Actor, Ticket: list[0].String(), Payload: raw}); err != nil {
		return nil, err
	}
	if err := s.syncer.Created(withoutKey(ctx), list, budgetOf); err != nil {
		log.Printf("istari: plan %v: mirroring: %v; the next sync repairs it", out, err)
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

// PlanAnswer is what istari plan submit answers.
type PlanAnswer struct {
	Size     string   `json:"size,omitempty"`
	Children []string `json:"children,omitempty"`
}

// submitPlan checks, records and, for a split, carries out the planner's
// plan. What code rejects fails the run, except a split child that misses
// the ticket standard: the planner fixes it in the same run (spec 4.3).
func (s *Server) submitPlan(ctx context.Context, r PlanSubmitRequest) (any, error) {
	run, isAgent := runOf(ctx)
	switch {
	case !isAgent:
		return nil, &core.Error{Code: runner.CodeTokenInvalid, Message: "istari plan submit is for the planner: it needs the run token in ISTARI_TOKEN"}
	case run.Role != "planner":
		return nil, &core.Error{Code: CodeTokenForbidden, Message: "only the planner submits a plan, not the " + run.Role}
	case s.plans == nil:
		return nil, &core.Error{Code: core.CodeInvalidState, Message: "planning is off: istari has no workspaces"}
	}
	answer, err := s.applyPlan(ctx, run, r.Plan)
	if err != nil {
		if errors.Is(err, runner.ErrChildStandard) {
			return nil, &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
		}
		if _, rerr := s.runs.Reject(ctx, run.ID, err.Error()); rerr != nil {
			return nil, rerr
		}
		return nil, &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
	}
	return answer, nil
}

func (s *Server) applyPlan(ctx context.Context, run runner.Run, raw string) (PlanAnswer, error) {
	res, err := runner.ParsePlanResult([]byte(raw))
	if err != nil {
		return PlanAnswer{}, err
	}
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return PlanAnswer{}, err
	}
	settings, err := s.engine.SettingsOf(ctx, ref.Repo)
	if err != nil {
		return PlanAnswer{}, err
	}
	if tk, err := s.tickets.Get(ctx, run.Ticket); err != nil || tk.State != core.TicketPlanning {
		if err != nil {
			return PlanAnswer{}, err
		}
		return PlanAnswer{}, fmt.Errorf("plan: %s is %s; a plan is submitted in planning", run.Ticket, tk.State)
	}
	ps := runner.PlanSettings{MaxSize: settings.MaxSize, Gate: settings.PlanGate}
	if res, err = s.plans.Check(ctx, run, res, ps); err != nil {
		return PlanAnswer{}, err
	}
	answer := PlanAnswer{Size: res.Size}
	if res.Split != nil && len(res.Split.Tickets) > 0 {
		// The split goes first: if it fails, the journal has no plan for it.
		if answer.Children, err = s.plans.Split(withoutKey(ctx), run, *res.Split); err != nil {
			return PlanAnswer{}, err
		}
	}
	if err := s.plans.Record(ctx, run, res, ps); err != nil {
		return PlanAnswer{}, err
	}
	return answer, nil
}
