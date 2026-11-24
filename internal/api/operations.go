package api

import (
	"context"
	"errors"

	"github.com/sbraznikov/istari/internal/core"
)

// Requests of the operations. Actor names who acts: on the local socket the
// caller says who it is; remote authentication comes later (spec section 11).
type (
	TaskCreateRequest struct {
		Actor string `json:"actor"`
		core.NewTask
	}
	TaskRef struct {
		Actor string `json:"actor"`
		ID    string `json:"id"`
	}
	TaskClaimRequest struct {
		Actor string `json:"actor"`
		ID    string `json:"id,omitempty"`   // claim this task, or
		Role  string `json:"role,omitempty"` // the oldest ready task of this role
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
	TicketRef struct {
		Actor string `json:"actor"`
		Ref   string `json:"ref"`
	}
	TicketFireRequest struct {
		Actor   string `json:"actor"`
		Ref     string `json:"ref"`
		Trigger string `json:"trigger"`
	}
	TicketHoldRequest struct {
		Actor  string `json:"actor"`
		Ref    string `json:"ref"`
		Reason string `json:"reason"`
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
func (r *TaskRef) validate() error             { return needActor(r.Actor) }
func (r *TaskClaimRequest) validate() error    { return needActor(r.Actor) }
func (r *LeaseRequest) validate() error        { return needActor(r.Lease.Owner) }
func (r *TicketCreateRequest) validate() error { return needActor(r.Actor) }
func (r *TicketFireRequest) validate() error   { return needActor(r.Actor) }
func (r *TicketHoldRequest) validate() error   { return needActor(r.Actor) }

// TicketRef and TaskRef also serve reads (show), which need no actor.
type (
	TaskShowRequest struct {
		ID string `json:"id"`
	}
	TicketShowRequest struct {
		Ref string `json:"ref"`
	}
)

func (r *TicketRef) validate() error { return needActor(r.Actor) }

// OK is the response of operations that return nothing else.
type OK struct {
	OK bool `json:"ok"`
}

func (s *Server) operations() map[string]operation {
	t, k := s.tasks, s.tickets
	ok := func(err error) (any, error) {
		if err != nil {
			return nil, err
		}
		return OK{OK: true}, nil
	}
	return map[string]operation{
		"task.create": op(func(ctx context.Context, r TaskCreateRequest) (any, error) { return t.Create(ctx, r.Actor, r.NewTask) }),
		"task.list":   op(func(ctx context.Context, _ Empty) (any, error) { return t.List(ctx) }),
		"task.show":   op(func(ctx context.Context, r TaskShowRequest) (any, error) { return t.Get(ctx, r.ID) }),
		"task.claim": op(func(ctx context.Context, r TaskClaimRequest) (any, error) {
			if r.ID != "" {
				return t.Claim(ctx, r.Actor, r.ID)
			}
			return t.ClaimReady(ctx, r.Actor, r.Role)
		}),
		"task.renew":    op(func(ctx context.Context, r LeaseRequest) (any, error) { return t.Renew(ctx, r.Lease) }),
		"task.release":  op(func(ctx context.Context, r LeaseRequest) (any, error) { return ok(t.Release(ctx, r.Lease)) }),
		"task.complete": op(func(ctx context.Context, r LeaseRequest) (any, error) { return ok(t.Complete(ctx, r.Lease)) }),
		"task.fail":     op(func(ctx context.Context, r LeaseRequest) (any, error) { return ok(t.Fail(ctx, r.Lease, r.Reason)) }),
		"task.block":    op(func(ctx context.Context, r LeaseRequest) (any, error) { return ok(t.Block(ctx, r.Lease, r.Kind)) }),
		"task.unblock":  op(func(ctx context.Context, r TaskRef) (any, error) { return ok(t.Unblock(ctx, r.Actor, r.ID)) }),
		"task.cancel":   op(func(ctx context.Context, r TaskRef) (any, error) { return ok(t.Cancel(ctx, r.Actor, r.ID)) }),

		"ticket.create": op(func(ctx context.Context, r TicketCreateRequest) (any, error) {
			return k.Create(ctx, r.Actor, r.Ref, r.Project)
		}),
		"ticket.list": op(func(ctx context.Context, _ Empty) (any, error) { return k.List(ctx) }),
		"ticket.show": op(func(ctx context.Context, r TicketShowRequest) (any, error) { return k.Get(ctx, r.Ref) }),
		"ticket.fire": op(func(ctx context.Context, r TicketFireRequest) (any, error) {
			return k.Fire(ctx, r.Actor, r.Ref, r.Trigger)
		}),
		"ticket.hold": op(func(ctx context.Context, r TicketHoldRequest) (any, error) {
			return k.SetHold(ctx, r.Actor, r.Ref, r.Reason)
		}),
		"ticket.unhold": op(func(ctx context.Context, r TicketRef) (any, error) { return k.ClearHold(ctx, r.Actor, r.Ref) }),
	}
}
