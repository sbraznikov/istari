package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Requests of the operations. Actor names who acts: on the local socket the
// caller says who it is; remote authentication comes later (spec section 11).
type (
	TaskCreateRequest struct {
		Actor string `json:"actor"`
		core.NewTask
	}
	TaskShowRequest struct {
		ID string `json:"id"`
	}
	TaskActRequest struct {
		Actor string `json:"actor"`
		ID    string `json:"id"`
	}
	TaskClaimRequest struct {
		Actor string `json:"actor"`
		ID    string `json:"id,omitempty"`   // claim this task, or
		Role  string `json:"role,omitempty"` // the oldest ready task of this role ("" = any)
	}
	LeaseRequest struct {
		Lease  core.Lease `json:"lease"`
		Reason string     `json:"reason,omitempty"` // fail
		Kind   string     `json:"kind,omitempty"`   // block
	}
	TicketCreateRequest struct {
		Actor   string `json:"actor"`
		Ref     string `json:"ref"`
		Project string `json:"project,omitempty"`
	}
	TicketShowRequest struct {
		Ref string `json:"ref"`
	}
	TicketFireRequest struct {
		Actor   string `json:"actor"`
		Ref     string `json:"ref"`
		Trigger string `json:"trigger"`
	}
	TicketHoldRequest struct {
		Actor  string `json:"actor"`
		Ref    string `json:"ref"`
		Reason string `json:"reason"` // hold: the reason; unhold: only a hold of this reason ("" = any)
	}
	DispatchRequest struct {
		Actor   string `json:"actor"`
		Project string `json:"project,omitempty"` // empty: all projects
	}
	TimerScheduleRequest struct {
		Actor   string          `json:"actor"`
		In      string          `json:"in,omitempty"` // a Go duration from the daemon's now, or
		At      time.Time       `json:"at,omitzero"`  // a moment
		Op      string          `json:"op"`
		Request json.RawMessage `json:"request"`
	}
	TicketReadyRequest struct {
		Actor string `json:"actor"`
		Ref   string `json:"ref"`
	}
	TicketPriorityRequest struct {
		Actor    string `json:"actor"`
		Ref      string `json:"ref"`
		Priority string `json:"priority"`
	}
	Empty struct{}
)

var errNoActor = errors.New("the request names no actor")

func needActor(actor string) error {
	if actor == "" {
		return errNoActor
	}
	return nil
}

func (r *TaskCreateRequest) validate() error   { return needActor(r.Actor) }
func (r *TaskActRequest) validate() error      { return needActor(r.Actor) }
func (r *TaskClaimRequest) validate() error    { return needActor(r.Actor) }
func (r *LeaseRequest) validate() error        { return needActor(r.Lease.Owner) }
func (r *TicketCreateRequest) validate() error { return needActor(r.Actor) }
func (r *TicketFireRequest) validate() error   { return needActor(r.Actor) }
func (r *TicketHoldRequest) validate() error   { return needActor(r.Actor) }
func (r *TicketReadyRequest) validate() error  { return needActor(r.Actor) }
func (r *DispatchRequest) validate() error     { return needActor(r.Actor) }
func (r *TimerScheduleRequest) validate() error {
	if r.In != "" {
		d, err := time.ParseDuration(r.In)
		if err != nil || d <= 0 || !r.At.IsZero() {
			return errors.New("in must be a positive duration, and at must then be empty")
		}
	} else if r.At.IsZero() {
		return errors.New("the request names no time: in or at")
	}
	return needActor(r.Actor)
}

// CodeSyncDisabled refuses istari sync when no tracker is configured.
const CodeSyncDisabled = "sync_disabled"

// OK is the response of operations that return nothing else.
type OK struct {
	OK bool `json:"ok"`
}

func ok(err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return OK{OK: true}, nil
}

func (s *Server) operations() map[string]operation {
	t, k := s.tasks, s.tickets
	// Answers to a write repeated with its idempotency key, built from the
	// current projections: the write itself is not performed again.
	okAgain := func(context.Context, requestRecord) (any, error) { return OK{OK: true}, nil }
	taskAgain := func(ctx context.Context, r requestRecord) (any, error) { return t.Get(ctx, r.Task) }
	leaseAgain := func(ctx context.Context, r requestRecord) (any, error) { return t.LeaseOf(ctx, r.Task, r.Actor) }
	ticketAgain := func(ctx context.Context, r requestRecord) (any, error) { return k.Get(ctx, r.Ticket) }
	timerAgain := func(ctx context.Context, r requestRecord) (any, error) {
		timers, err := s.sched.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, timer := range timers {
			if timer.ID == r.Timer {
				return timer, nil
			}
		}
		return nil, &core.Error{Code: core.CodeNotFound, Message: "timer " + r.Timer + " does not exist"}
	}

	return map[string]operation{
		"task.list": read(func(ctx context.Context, _ Empty) (any, error) { return t.List(ctx) }),
		"task.show": read(func(ctx context.Context, r TaskShowRequest) (any, error) { return t.Get(ctx, r.ID) }),
		"task.create": write(taskAgain, func(ctx context.Context, r TaskCreateRequest) (any, error) {
			return t.Create(ctx, r.Actor, r.NewTask)
		}),
		"task.claim": write(leaseAgain, func(ctx context.Context, r TaskClaimRequest) (any, error) {
			if r.ID != "" {
				return t.Claim(ctx, r.Actor, r.ID)
			}
			return t.ClaimReady(ctx, r.Actor, r.Role)
		}),
		"task.renew":    write(leaseAgain, func(ctx context.Context, r LeaseRequest) (any, error) { return t.Renew(ctx, r.Lease) }),
		"task.release":  write(okAgain, func(ctx context.Context, r LeaseRequest) (any, error) { return ok(t.Release(ctx, r.Lease)) }),
		"task.complete": write(okAgain, func(ctx context.Context, r LeaseRequest) (any, error) { return ok(t.Complete(ctx, r.Lease)) }),
		"task.fail":     write(okAgain, func(ctx context.Context, r LeaseRequest) (any, error) { return ok(t.Fail(ctx, r.Lease, r.Reason)) }),
		"task.block":    write(okAgain, func(ctx context.Context, r LeaseRequest) (any, error) { return ok(t.Block(ctx, r.Lease, r.Kind)) }),
		"task.unblock":  write(okAgain, func(ctx context.Context, r TaskActRequest) (any, error) { return ok(t.Unblock(ctx, r.Actor, r.ID)) }),
		"task.cancel":   write(okAgain, func(ctx context.Context, r TaskActRequest) (any, error) { return ok(t.Cancel(ctx, r.Actor, r.ID)) }),
		"task.retry":    write(okAgain, func(ctx context.Context, r TaskActRequest) (any, error) { return ok(t.Retry(ctx, r.Actor, r.ID)) }),

		"ticket.list": read(func(ctx context.Context, _ Empty) (any, error) { return k.List(ctx) }),
		"ticket.show": read(func(ctx context.Context, r TicketShowRequest) (any, error) { return k.Get(ctx, r.Ref) }),
		"ticket.create": write(ticketAgain, func(ctx context.Context, r TicketCreateRequest) (any, error) {
			return k.Create(ctx, r.Actor, r.Ref, r.Project)
		}),
		// ticket.fire never admits (spec 4.5, 7.7): admission checks trust
		// and the standard, so it goes through ticket.ready or the sync.
		"ticket.fire": write(ticketAgain, func(ctx context.Context, r TicketFireRequest) (any, error) {
			if r.Trigger == "accept" {
				return nil, &core.Error{Code: core.CodeInvalidTransition, Message: "a ticket is admitted with istari ticket ready, not by firing accept"}
			}
			return k.Fire(ctx, r.Actor, r.Ref, r.Trigger)
		}),
		"ticket.hold": write(ticketAgain, func(ctx context.Context, r TicketHoldRequest) (any, error) {
			return k.SetHold(ctx, r.Actor, r.Ref, r.Reason)
		}),
		// ticket.priority sets a ticket's priority (spec 4.9): humans only.
		// A synced ticket's label is written at once.
		"ticket.priority": write(ticketAgain, func(ctx context.Context, r TicketPriorityRequest) (any, error) {
			if s.syncer != nil {
				return s.syncer.SetPriority(ctx, r.Actor, r.Ref, r.Priority)
			}
			return k.SetPriority(ctx, r.Actor, r.Ref, r.Priority)
		}),
		"dispatch.pause": write(okAgain, func(ctx context.Context, r DispatchRequest) (any, error) { return ok(t.Pause(ctx, r.Actor, r.Project)) }),
		"dispatch.resume": write(okAgain, func(ctx context.Context, r DispatchRequest) (any, error) {
			return ok(t.Resume(ctx, r.Actor, r.Project))
		}),
		// ticket.ready admits a ticket by hand (spec 4.5): the actor must be
		// human:<tracker login> of a trusted member.
		"ticket.ready": write(ticketAgain, func(ctx context.Context, r TicketReadyRequest) (any, error) {
			if s.syncer == nil {
				return nil, &core.Error{Code: CodeSyncDisabled, Message: "no project has a ticket tracker"}
			}
			login, ok := strings.CutPrefix(r.Actor, "human:")
			if !ok || login == "" {
				return nil, &core.Error{Code: tickets.CodeNotTrusted, Message: "admission needs a human actor: human:<tracker login>"}
			}
			if err := s.syncer.Ready(ctx, login, r.Ref); err != nil {
				return nil, err
			}
			return k.Get(ctx, r.Ref)
		}),
		// sync.run polls the tracker now. It writes, but a repeat is harmless,
		// so it takes no idempotency key.
		"sync.run": read(func(ctx context.Context, _ Empty) (any, error) {
			if s.syncer == nil {
				return nil, &core.Error{Code: CodeSyncDisabled, Message: "no project has a ticket tracker to sync"}
			}
			return s.syncer.Sync(ctx)
		}),
		"timer.list": read(func(ctx context.Context, _ Empty) (any, error) { return s.sched.List(ctx) }),
		"timer.schedule": write(timerAgain, func(ctx context.Context, r TimerScheduleRequest) (any, error) {
			at := r.At
			if r.In != "" {
				d, _ := time.ParseDuration(r.In) // validated
				at = s.clock().Add(d)
			}
			return s.sched.Schedule(ctx, r.Actor, at, r.Op, r.Request)
		}),

		"ticket.unhold": write(ticketAgain, func(ctx context.Context, r TicketHoldRequest) (any, error) {
			return k.ClearHold(ctx, r.Actor, r.Ref, r.Reason)
		}),
	}
}
