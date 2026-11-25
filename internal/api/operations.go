package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/runner"
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
	ActorRequest struct {
		Actor string `json:"actor"`
	}
	RunRequest struct {
		ID string `json:"id"`
	}
	RunActRequest struct {
		Actor string `json:"actor"`
		ID    string `json:"id"`
	}
	RunLogsRequest struct {
		ID    string               `json:"id"`
		After runner.TranscriptPos `json:"after"` // from an earlier answer's next
	}
	PushRequest struct {
		Actor  string `json:"actor"`
		Branch string `json:"branch,omitempty"` // "": the ticket's own branch
		Force  bool   `json:"force,omitempty"`  // with a lease on what istari last saw
	}
	TicketPlanRequest struct {
		Actor string       `json:"actor"`
		Plan  tickets.Plan `json:"plan"`
	}
	PROpenRequest struct {
		Actor string `json:"actor"`
		Title string `json:"title"`
		Body  string `json:"body"` // Summary, Evidence, Merge Danger
	}
	PlanSubmitRequest struct {
		Actor string `json:"actor"`
		Plan  string `json:"plan"` // the YAML the planner wrote: code parses it
	}
	ReviewSubmitRequest struct {
		Actor  string `json:"actor"`
		Report string `json:"report"` // the YAML the reviewer wrote: code parses it
	}
	RefineSubmitRequest struct {
		Actor  string `json:"actor"`
		Result string `json:"result"` // the YAML the refiner wrote, as written: code parses it
	}
	CheckpointRequest struct {
		Actor   string `json:"actor"`
		Message string `json:"message"` // "<done>; <next>"
	}
	RunCompleteRequest struct {
		Actor   string `json:"actor"`
		Message string `json:"message,omitempty"`
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
	TicketResumeRequest struct {
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
func (r *ActorRequest) validate() error        { return needActor(r.Actor) }
func (r *RunActRequest) validate() error       { return needActor(r.Actor) }
func (r *RunCompleteRequest) validate() error  { return needActor(r.Actor) }
func (r *PushRequest) validate() error         { return needActor(r.Actor) }
func (r *CheckpointRequest) validate() error   { return needActor(r.Actor) }
func (r *RefineSubmitRequest) validate() error { return needActor(r.Actor) }
func (r *PlanSubmitRequest) validate() error   { return needActor(r.Actor) }
func (r *ReviewSubmitRequest) validate() error { return needActor(r.Actor) }
func (r *PROpenRequest) validate() error       { return needActor(r.Actor) }
func (r *TicketPlanRequest) validate() error   { return needActor(r.Actor) }
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

// orEmpty answers an empty list as [], not null.
func orEmpty(list []runner.Workspace, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []runner.Workspace{}
	}
	return list, nil
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
		"ticket.resume": write(ticketAgain, func(ctx context.Context, r TicketResumeRequest) (any, error) {
			return s.resume(ctx, r.Actor, r.Ref)
		}),
		"recover": write(okAgain, func(ctx context.Context, r ActorRequest) (any, error) {
			return s.recoverTickets(ctx, r.Actor)
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
		// Gates: the tasks a human performs (spec section 9).
		"gate.list": read(func(ctx context.Context, _ Empty) (any, error) {
			all, err := t.List(ctx)
			if err != nil {
				return nil, err
			}
			gates := []core.Task{}
			for _, task := range all {
				if task.Role == pipeline.RoleHuman && task.State == core.TaskReady {
					gates = append(gates, task)
				}
			}
			return gates, nil
		}),
		"gate.approve": write(taskAgain, func(ctx context.Context, r TaskActRequest) (any, error) {
			if !strings.HasPrefix(r.Actor, "human:") {
				return nil, &core.Error{Code: core.CodeHumanOnly, Message: "a gate is approved by a human, not " + r.Actor}
			}
			task, err := t.Get(ctx, r.ID)
			if err != nil {
				return nil, err
			}
			if task.Role != pipeline.RoleHuman {
				return nil, &core.Error{Code: core.CodeInvalidTask, Message: r.ID + " is a " + task.Role + " task, not a gate"}
			}
			lease, err := t.Claim(ctx, r.Actor, r.ID)
			if err != nil {
				return nil, err
			}
			if err := t.Complete(ctx, lease); err != nil {
				return nil, err
			}
			return t.Get(ctx, r.ID)
		}),
		// Runs (spec sections 7.2 and 9).
		"run.list": read(func(ctx context.Context, _ Empty) (any, error) { return s.runs.List(ctx) }),
		// The slots paused because their harness did not start (spec 7.3).
		"runner.slots": read(func(context.Context, Empty) (any, error) {
			if s.runner == nil {
				return nil, nil
			}
			return s.runner.Paused(), nil
		}),
		"run.show": read(func(ctx context.Context, r RunRequest) (any, error) { return s.runs.Get(ctx, r.ID) }),
		"run.logs": read(func(ctx context.Context, r RunLogsRequest) (any, error) { return s.runLogs(ctx, r) }),
		"run.stop": write(okAgain, func(ctx context.Context, r RunActRequest) (any, error) {
			if s.runner == nil {
				return nil, &core.Error{Code: core.CodeInvalidState, Message: "the runner is off"}
			}
			return ok(s.runner.Stop(ctx, r.ID, r.Actor))
		}),
		// run.complete is istari complete: the agent's word that its work is
		// done; the runner completes the task when the harness exits.
		"run.complete": write(okAgain, func(ctx context.Context, r RunCompleteRequest) (any, error) {
			run, isAgent := runOf(ctx)
			if !isAgent {
				return nil, &core.Error{Code: runner.CodeTokenInvalid, Message: "istari complete is for an agent: it needs the run token in ISTARI_TOKEN"}
			}
			if run.Role == "reviewer" {
				// The verdict needs the report (spec 7.6): a reviewer that
				// ends without one fails its run, and the attempt counts.
				if _, submitted, err := s.reviewOf(ctx, run.Ticket, run.Task); err != nil {
					return nil, err
				} else if !submitted {
					return nil, &core.Error{Code: core.CodeInvalidState, Message: "submit the review with istari review submit before istari complete"}
				}
			}
			_, err := s.runs.Complete(ctx, run.ID, r.Message)
			return ok(err)
		}),
		// vcs.push pushes an agent's work to its ticket branch, and only
		// there (spec sections 2.1 and 11): the implementer's run alone.
		"vcs.push": write(okAgain, func(ctx context.Context, r PushRequest) (any, error) { return ok(s.push(ctx, r)) }),
		"whoami":   read(func(ctx context.Context, _ Empty) (any, error) { return s.whoami(ctx) }),
		// pr.open is istari pr open (spec 7.7): the full checks run by code,
		// the description checked, a draft linked to the ticket.
		"pr.open": write(okAgain, func(ctx context.Context, r PROpenRequest) (any, error) {
			run, isAgent := runOf(ctx)
			switch {
			case !isAgent:
				return nil, &core.Error{Code: runner.CodeTokenInvalid, Message: "istari pr open is for the implementer: it needs the run token in ISTARI_TOKEN"}
			case run.Role != "implementer":
				return nil, &core.Error{Code: CodeTokenForbidden, Message: "only the implementer opens a pull request, not the " + run.Role}
			case s.prs == nil:
				return nil, &core.Error{Code: core.CodeInvalidState, Message: "pull requests are off: istari has no GitHub access"}
			}
			pr, err := s.prs.Open(ctx, run, r.Title, r.Body)
			if err != nil {
				return nil, &core.Error{Code: core.CodeInvalidState, Message: err.Error()}
			}
			return pr, nil
		}),
		// plan.submit is istari plan submit (spec 4.9): code checks the plan
		// against the repository and the ticket, records it, and splits.
		"plan.submit": write(okAgain, func(ctx context.Context, r PlanSubmitRequest) (any, error) { return s.submitPlan(ctx, r) }),
		// review.submit is istari review submit (spec 7.6): code checks the
		// findings and records the report; the verdict is the pipeline's.
		"review.submit": write(okAgain, func(ctx context.Context, r ReviewSubmitRequest) (any, error) { return s.submitReview(ctx, r) }),
		// refine.submit is the refiner's structured result (spec 4.5): code
		// checks and applies it; what it rejects fails the run.
		"refine.submit": write(okAgain, func(ctx context.Context, r RefineSubmitRequest) (any, error) {
			run, isAgent := runOf(ctx)
			switch {
			case !isAgent:
				return nil, &core.Error{Code: runner.CodeTokenInvalid, Message: "istari refine submit is for the refiner: it needs the run token in ISTARI_TOKEN"}
			case run.Role != "refiner":
				return nil, &core.Error{Code: CodeTokenForbidden, Message: "only the refiner submits a refine result, not the " + run.Role}
			case s.refines == nil:
				return nil, &core.Error{Code: core.CodeInvalidState, Message: "refining is off: istari has no ticket tracker"}
			}
			res, err := runner.ParseRefine([]byte(r.Result))
			if err == nil {
				err = s.refines.Apply(ctx, run, res)
			}
			if err != nil {
				if _, rerr := s.runs.Reject(ctx, run.ID, err.Error()); rerr != nil {
					return nil, rerr
				}
				return nil, &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
			}
			return OK{OK: true}, nil
		}),
		// checkpoint is istari checkpoint (spec sections 4.6 and 7.7): commit,
		// checks run by code, push, journal entry. The implementer's.
		"checkpoint": write(okAgain, func(ctx context.Context, r CheckpointRequest) (any, error) {
			run, isAgent := runOf(ctx)
			switch {
			case !isAgent:
				return nil, &core.Error{Code: runner.CodeTokenInvalid, Message: "istari checkpoint is for an agent: it needs the run token in ISTARI_TOKEN"}
			case run.Role != "implementer":
				return nil, &core.Error{Code: CodeTokenForbidden, Message: "only the implementer makes checkpoints, not the " + run.Role}
			case s.saves == nil:
				return nil, &core.Error{Code: core.CodeInvalidState, Message: "checkpoints are off: istari has no workspaces"}
			}
			e, err := s.saves.Make(ctx, run, r.Message)
			if err != nil {
				return nil, err
			}
			return e.Checkpoint, nil
		}),
		// ticket.plan is istari ticket create (spec section 4.9): a set of
		// linked tickets, validated as a whole before the first write.
		"ticket.plan": write(func(ctx context.Context, done requestRecord) (any, error) { return s.planAgain(ctx, done) },
			func(ctx context.Context, r TicketPlanRequest) (any, error) { return s.createPlan(ctx, r) }),
		// Ticket worktrees (spec section 7.2).
		"workspace.list": read(func(ctx context.Context, _ Empty) (any, error) {
			if s.spaces == nil {
				return []runner.Workspace{}, nil
			}
			return s.spaces.List(ctx)
		}),
		"workspace.orphans": read(func(ctx context.Context, _ Empty) (any, error) {
			if s.spaces == nil {
				return []runner.Workspace{}, nil
			}
			all, err := k.List(ctx)
			if err != nil {
				return nil, err
			}
			return orEmpty(s.spaces.Orphans(ctx, all))
		}),
		"workspace.prune": write(okAgain, func(ctx context.Context, _ ActorRequest) (any, error) {
			if s.spaces == nil {
				return []runner.Workspace{}, nil
			}
			all, err := k.List(ctx)
			if err != nil {
				return nil, err
			}
			return orEmpty(s.spaces.Prune(ctx, all))
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
