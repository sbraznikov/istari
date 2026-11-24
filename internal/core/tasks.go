package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sbraznikov/istari/internal/store"
)

// TaskState is a state of the task machine (spec section 4.2).
type TaskState string

// Task states.
const (
	TaskWaiting   TaskState = "waiting"
	TaskReady     TaskState = "ready"
	TaskClaimed   TaskState = "claimed"
	TaskBlocked   TaskState = "blocked"
	TaskCompleted TaskState = "completed"
	TaskFailed    TaskState = "failed"
	TaskCanceled  TaskState = "canceled"
)

// Defaults from spec section 4.2.
const (
	DefaultLease       = 5 * time.Minute
	DefaultMaxAttempts = 3
)

const taskKind = "task"

// Task is the projection of one task.
type Task struct {
	ID          string    `json:"id"`
	Ticket      string    `json:"ticket"`
	Project     string    `json:"project,omitempty"`
	Role        string    `json:"role"`
	State       TaskState `json:"state"`
	Needs       []string  `json:"needs,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	LeaseUntil  time.Time `json:"lease_until,omitzero"`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	BlockKind   string    `json:"block_kind,omitempty"`
	CreatedAt   time.Time `json:"created_at"`

	token   string // current claim token; private to the owner's Lease
	version int64  // store record version
}

// taskJSON is the stored form: Task plus the claim token, which the public
// JSON of a task must not reveal.
type taskJSON struct {
	Task
	Token string `json:"token,omitempty"`
}

// Lease is what a successful claim hands to its owner. Every owner action
// presents it; a lease that is no longer current fails with claim_lost.
type Lease struct {
	Task  string    `json:"task"`
	Owner string    `json:"owner"`
	Token string    `json:"token"`
	Until time.Time `json:"until"`
}

// NewTask describes a task to create.
type NewTask struct {
	ID          string // optional; generated when empty
	Ticket      string // required: no ticket, no work (spec section 2.1)
	Project     string
	Role        string
	Needs       []string
	MaxAttempts int // 0 means the configured default
}

// TaskOptions configures Tasks. Zero values take the spec defaults.
type TaskOptions struct {
	Clock       func() time.Time
	Lease       time.Duration
	MaxAttempts int
	Limits      Limits
	// DefaultPriority is the priority of a ticket without one; empty means
	// DefaultPriority (P2).
	DefaultPriority string
}

// Tasks runs the task state machine on top of a store. All writes go through
// store.Append, one event per operation, with the task change conditional on
// the version that was read, so concurrent operations cannot both succeed.
type Tasks struct {
	store store.Store
	opts  TaskOptions
}

// NewTasks returns the task machine.
func NewTasks(s store.Store, opts TaskOptions) *Tasks {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	clock := opts.Clock
	opts.Clock = func() time.Time { return clock().UTC() }
	if opts.Lease <= 0 {
		opts.Lease = DefaultLease
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.DefaultPriority == "" {
		opts.DefaultPriority = DefaultPriority
	}
	return &Tasks{store: s, opts: opts}
}

// Get returns one task.
func (t *Tasks) Get(ctx context.Context, id string) (Task, error) {
	rec, err := t.store.Get(ctx, taskKind, id)
	if errors.Is(err, store.ErrNotFound) {
		return Task{}, errorf(CodeNotFound, "task %s does not exist", id)
	}
	if err != nil {
		return Task{}, err
	}
	return decodeTask(rec)
}

// List returns all tasks ordered by id.
func (t *Tasks) List(ctx context.Context) ([]Task, error) {
	recs, err := t.store.List(ctx, taskKind)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(recs))
	for _, rec := range recs {
		task, err := decodeTask(rec)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// Create creates a task. It is ready when it has no needs or all of them are
// completed, and waiting otherwise. Needs may only name existing tasks, so the
// only cycle they can form is a task naming itself, which is rejected.
func (t *Tasks) Create(ctx context.Context, actor string, nt NewTask) (Task, error) {
	if nt.Ticket == "" {
		return Task{}, errorf(CodeInvalidTask, "a task needs a ticket")
	}
	if nt.Role == "" {
		return Task{}, errorf(CodeInvalidTask, "a task needs a role")
	}
	if nt.ID == "" {
		nt.ID = "t_" + randomHex(6)
	}
	for i, id := range nt.Needs {
		if id == nt.ID {
			return Task{}, errorf(CodeInvalidNeeds, "task %s needs itself", nt.ID)
		}
		if slices.Contains(nt.Needs[:i], id) {
			return Task{}, errorf(CodeInvalidNeeds, "task %s needs %s twice", nt.ID, id)
		}
	}
	task := Task{
		ID: nt.ID, Ticket: nt.Ticket, Project: nt.Project, Role: nt.Role,
		Needs: slices.Clone(nt.Needs), MaxAttempts: nt.MaxAttempts, CreatedAt: t.opts.Clock(),
	}
	if task.MaxAttempts <= 0 {
		task.MaxAttempts = t.opts.MaxAttempts
	}
	err := retry(func() error {
		if _, err := t.Get(ctx, task.ID); err == nil {
			return errorf(CodeInvalidTask, "task %s already exists", task.ID)
		} else if !isCode(err, CodeNotFound) {
			return err
		}
		task.State = TaskReady
		var guards []store.Change
		for _, id := range task.Needs {
			need, err := t.Get(ctx, id)
			if isCode(err, CodeNotFound) {
				return errorf(CodeInvalidNeeds, "task %s needs %s, which does not exist", task.ID, id)
			}
			if err != nil {
				return err
			}
			if need.State != TaskCompleted {
				task.State = TaskWaiting
			}
			// Rewriting the need unchanged makes a concurrent completion of it
			// conflict with this create, so the new task cannot miss it.
			guards = append(guards, mustChange(need))
		}
		return t.commit(ctx, actor, "task.created", task, map[string]any{"state": task.State}, guards...)
	})
	if err != nil {
		return Task{}, err
	}
	return task, nil
}

// Claim claims one ready task for actor.
func (t *Tasks) Claim(ctx context.Context, actor, id string) (Lease, error) {
	var lease Lease
	err := retry(func() error {
		task, err := t.Get(ctx, id)
		if err != nil {
			return err
		}
		lease, err = t.claim(ctx, actor, task)
		return err
	})
	return lease, err
}

// ClaimReady claims a ready task of the role, or of any role when role is
// empty: the one whose ticket has the highest priority, the oldest on a tie
// (spec 4.9). It fails with no_ready_task when there is none.
func (t *Tasks) ClaimReady(ctx context.Context, actor, role string) (Lease, error) {
	return t.ClaimReadyIn(ctx, actor, role, nil)
}

// ClaimReadyIn is ClaimReady among the tasks of the named projects only; no
// projects means every project. The runner takes only the work of the
// projects it has repositories for.
func (t *Tasks) ClaimReadyIn(ctx context.Context, actor, role string, projects []string) (Lease, error) {
	var lease Lease
	err := retry(func() error {
		all, err := t.List(ctx)
		if err != nil {
			return err
		}
		onHold, err := t.heldTickets(ctx)
		if err != nil {
			return err
		}
		var ready []Task
		for _, task := range all {
			if task.State == TaskReady && (role == "" || task.Role == role) && (projects == nil || slices.Contains(projects, task.Project)) && !onHold[task.Ticket] {
				ready = append(ready, task)
			}
		}
		if len(ready) == 0 {
			return errorf(CodeNoReadyTask, "no ready task for role %s", role)
		}
		priority, err := t.priorities(ctx, all)
		if err != nil {
			return err
		}
		slices.SortStableFunc(ready, func(a, b Task) int {
			if c := strings.Compare(priority(a), priority(b)); c != 0 {
				return c
			}
			return a.CreatedAt.Compare(b.CreatedAt)
		})
		// A task that a pause or a full limit keeps back is skipped for the
		// next one. A conflict re-reads the list: another worker was faster.
		var held error
		for _, task := range ready {
			lease, err = t.claim(ctx, actor, task)
			if isCode(err, CodePaused) || isCode(err, CodeLimitReached) {
				if held == nil {
					held = err
				}
				continue
			}
			return err
		}
		return held
	})
	return lease, err
}

// heldTickets returns the tickets on hold: their tasks are not handed out
// (spec section 7.2: a run starts only when there is no hold on the ticket).
func (t *Tasks) heldTickets(ctx context.Context) (map[string]bool, error) {
	recs, err := t.store.List(ctx, ticketKind)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, rec := range recs {
		ticket, err := decodeTicket(rec)
		if err != nil {
			return nil, err
		}
		if ticket.Hold != "" {
			held[ticket.Ref] = true
		}
	}
	return held, nil
}

// priorities returns the priority a task is handed out at: its ticket's, or
// higher when an unfinished task of a more urgent ticket needs it, directly or
// through other tasks (spec 4.9), so urgent work does not wait behind it.
func (t *Tasks) priorities(ctx context.Context, all []Task) (func(Task) string, error) {
	recs, err := t.store.List(ctx, ticketKind)
	if err != nil {
		return nil, err
	}
	byTicket := map[string]string{}
	for _, rec := range recs {
		ticket, err := decodeTicket(rec)
		if err != nil {
			return nil, err
		}
		byTicket[ticket.Ref] = ticket.Priority
	}
	neededBy := map[string][]Task{}
	for _, task := range all {
		if task.State == TaskCompleted || task.State == TaskFailed || task.State == TaskCanceled {
			continue
		}
		for _, id := range task.Needs {
			neededBy[id] = append(neededBy[id], task)
		}
	}
	memo := map[string]string{}
	var priority func(Task) string
	priority = func(task Task) string {
		if p, ok := memo[task.ID]; ok {
			return p
		}
		p := byTicket[task.Ticket]
		if p == "" {
			p = t.opts.DefaultPriority
		}
		for _, other := range neededBy[task.ID] {
			p = min(p, priority(other)) // needs form no cycles: a task needs only existing tasks
		}
		memo[task.ID] = p
		return p
	}
	return priority, nil
}

// claim claims a task read at task.version. A store conflict is returned as
// is: the caller re-reads, which tells a competing claim (task_claimed) from
// an unrelated change (claim again).
func (t *Tasks) claim(ctx context.Context, actor string, task Task) (Lease, error) {
	switch task.State {
	case TaskReady:
	case TaskClaimed:
		return Lease{}, errorf(CodeTaskClaimed, "task %s is claimed by %s", task.ID, task.Owner)
	default:
		return Lease{}, errorf(CodeInvalidState, "task %s is %s, not ready", task.ID, task.State)
	}
	admitted, err := t.admit(ctx, task)
	if err != nil {
		return Lease{}, err
	}
	task.State = TaskClaimed
	task.Owner = actor
	task.token = randomHex(16)
	task.LeaseUntil = t.opts.Clock().Add(t.opts.Lease)
	if err := t.commit(ctx, actor, "task.claimed", task, map[string]any{"lease_until": task.LeaseUntil}, admitted); err != nil {
		return Lease{}, err
	}
	return Lease{Task: task.ID, Owner: actor, Token: task.token, Until: task.LeaseUntil}, nil
}

// LeaseOf returns the current lease on a task if owner holds it, and
// claim_lost otherwise. It lets an owner recover its lease, e.g. when a
// claim is repeated with the same idempotency key.
func (t *Tasks) LeaseOf(ctx context.Context, id, owner string) (Lease, error) {
	task, err := t.Get(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	if task.State != TaskClaimed || task.Owner != owner || !t.opts.Clock().Before(task.LeaseUntil) {
		return Lease{}, errorf(CodeClaimLost, "%s holds no claim on task %s", owner, id)
	}
	return Lease{Task: id, Owner: owner, Token: task.token, Until: task.LeaseUntil}, nil
}

// Renew extends the owner's lease by the lease duration from now.
func (t *Tasks) Renew(ctx context.Context, lease Lease) (Lease, error) {
	err := t.asOwner(ctx, lease, "task.renewed", func(task *Task) (map[string]any, []store.Change, error) {
		task.LeaseUntil = t.opts.Clock().Add(t.opts.Lease)
		lease.Until = task.LeaseUntil
		return map[string]any{"lease_until": task.LeaseUntil}, nil, nil
	})
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// Release gives the task back: it returns to ready without using an attempt.
func (t *Tasks) Release(ctx context.Context, lease Lease) error {
	return t.asOwner(ctx, lease, "task.released", func(task *Task) (map[string]any, []store.Change, error) {
		task.State = TaskReady
		task.release()
		return nil, nil, nil
	})
}

// blockKinds are the reasons a task can be blocked (spec section 4.2).
var blockKinds = []string{"needs_input", "awaiting_approval", "budget_exceeded", "ci_red"}

// Block stops work on the owner's task for a reason: needs_input,
// awaiting_approval, budget_exceeded or ci_red. The claim ends; Unblock
// returns the task to ready.
func (t *Tasks) Block(ctx context.Context, lease Lease, kind string) error {
	if !slices.Contains(blockKinds, kind) {
		return errorf(CodeInvalidTask, "block kind %q is not one of %s", kind, strings.Join(blockKinds, ", "))
	}
	return t.asOwner(ctx, lease, "task.blocked", func(task *Task) (map[string]any, []store.Change, error) {
		task.State = TaskBlocked
		task.BlockKind = kind
		task.release()
		return map[string]any{"kind": kind}, nil, nil
	})
}

// Complete marks the owner's task completed and readies every waiting task
// whose needs are now all completed, in the same event.
func (t *Tasks) Complete(ctx context.Context, lease Lease) error {
	return t.asOwner(ctx, lease, "task.completed", func(task *Task) (map[string]any, []store.Change, error) {
		task.State = TaskCompleted
		task.release()
		readied, changes, err := t.dependents(ctx, task.ID)
		if err != nil {
			return nil, nil, err
		}
		return map[string]any{"readied": readied}, changes, nil
	})
}

// Fail records a failed attempt. Below the attempt limit the task returns to
// ready (task.failed); at the limit it stays failed and the one event is
// task.attempts_exhausted, which asks for the ticket hold attempts_exhausted.
func (t *Tasks) Fail(ctx context.Context, lease Lease, reason string) error {
	return t.asOwnerTyped(ctx, lease, func(task *Task) (string, map[string]any, []store.Change, error) {
		task.Attempts++
		task.release()
		typ := "task.failed"
		task.State = TaskReady
		if task.Attempts >= task.MaxAttempts {
			typ = "task.attempts_exhausted"
			task.State = TaskFailed
		}
		return typ, map[string]any{"reason": reason, "attempts": task.Attempts, "max_attempts": task.MaxAttempts}, nil, nil
	})
}

// Unblock returns a blocked task to ready.
func (t *Tasks) Unblock(ctx context.Context, actor, id string) error {
	return t.byActor(ctx, actor, id, "task.unblocked", func(task *Task) (map[string]any, error) {
		if task.State != TaskBlocked {
			return nil, errorf(CodeInvalidState, "task %s is %s, not blocked", id, task.State)
		}
		kind := task.BlockKind
		task.State = TaskReady
		task.BlockKind = ""
		return map[string]any{"kind": kind}, nil
	})
}

// Retry returns a failed task to ready with no attempts used. It is the
// human's answer to attempts_exhausted (spec sections 4.1 and 10.5).
func (t *Tasks) Retry(ctx context.Context, actor, id string) error {
	return t.byActor(ctx, actor, id, "task.retried", func(task *Task) (map[string]any, error) {
		if task.State != TaskFailed {
			return nil, errorf(CodeInvalidState, "task %s is %s, not failed", id, task.State)
		}
		payload := map[string]any{"attempts": task.Attempts}
		task.State = TaskReady
		task.Attempts = 0
		return payload, nil
	})
}

// Cancel cancels a task from any open state.
func (t *Tasks) Cancel(ctx context.Context, actor, id string) error {
	return t.byActor(ctx, actor, id, "task.canceled", func(task *Task) (map[string]any, error) {
		switch task.State {
		case TaskWaiting, TaskReady, TaskClaimed, TaskBlocked:
		default:
			return nil, errorf(CodeInvalidState, "task %s is %s and cannot be canceled", id, task.State)
		}
		task.State = TaskCanceled
		task.release()
		return nil, nil
	})
}

// ExpireLeases returns every claimed task whose lease deadline has passed by
// the clock to ready, with one task.expired event each, and returns their ids.
// The scheduler calls it on every tick; owners lose the lease at the deadline
// whether or not this has run yet.
func (t *Tasks) ExpireLeases(ctx context.Context) ([]string, error) {
	all, err := t.List(ctx)
	if err != nil {
		return nil, err
	}
	expired := []string{}
	for _, listed := range all {
		if listed.State != TaskClaimed {
			continue
		}
		done := false
		err := t.byActor(ctx, "system", listed.ID, "task.expired", func(task *Task) (map[string]any, error) {
			if task.State != TaskClaimed || t.opts.Clock().Before(task.LeaseUntil) {
				return nil, errNothingToDo // renewed, completed or released meanwhile
			}
			payload := map[string]any{"owner": task.Owner, "lease_until": task.LeaseUntil}
			task.State = TaskReady
			task.release()
			done = true
			return payload, nil
		})
		if err != nil && !errors.Is(err, errNothingToDo) {
			return expired, err
		}
		if done && err == nil {
			expired = append(expired, listed.ID)
		}
	}
	return expired, nil
}

// errNothingToDo ends a byActor operation without writing anything.
var errNothingToDo = errors.New("nothing to do")

// mutation changes a task read from the store and returns the event payload
// and any further changes to commit with it.
type mutation func(task *Task) (payload map[string]any, more []store.Change, err error)

// asOwner runs an owner action: it re-reads the task, checks the lease, applies
// mutate and commits one event of type typ, retrying on store conflicts.
func (t *Tasks) asOwner(ctx context.Context, lease Lease, typ string, mutate mutation) error {
	return t.asOwnerTyped(ctx, lease, func(task *Task) (string, map[string]any, []store.Change, error) {
		payload, more, err := mutate(task)
		return typ, payload, more, err
	})
}

func (t *Tasks) asOwnerTyped(ctx context.Context, lease Lease, mutate func(*Task) (string, map[string]any, []store.Change, error)) error {
	return retry(func() error {
		task, err := t.owned(ctx, lease)
		if err != nil {
			return err
		}
		typ, payload, more, err := mutate(&task)
		if err != nil {
			return err
		}
		return t.commit(ctx, lease.Owner, typ, task, payload, more...)
	})
}

// byActor runs an action that needs no lease, such as unblock or cancel.
func (t *Tasks) byActor(ctx context.Context, actor, id, typ string, mutate func(task *Task) (map[string]any, error)) error {
	return retry(func() error {
		task, err := t.Get(ctx, id)
		if err != nil {
			return err
		}
		payload, err := mutate(&task)
		if err != nil {
			return err
		}
		return t.commit(ctx, actor, typ, task, payload)
	})
}

// owned returns the task the lease is for, or claim_lost if the lease is no
// longer the task's current claim or its deadline has passed.
func (t *Tasks) owned(ctx context.Context, lease Lease) (Task, error) {
	task, err := t.Get(ctx, lease.Task)
	if err != nil {
		return Task{}, err
	}
	if task.State != TaskClaimed || task.token == "" || task.token != lease.Token {
		return Task{}, errorf(CodeClaimLost, "the claim on task %s is no longer held", lease.Task)
	}
	if !t.opts.Clock().Before(task.LeaseUntil) {
		return Task{}, errorf(CodeClaimLost, "the lease on task %s expired at %s", lease.Task, task.LeaseUntil)
	}
	return task, nil
}

// dependents returns a change for every waiting task that needs done: the ones
// whose needs are now all completed become ready (their ids are returned), the
// others are rewritten unchanged. Writing every dependent makes two needs that
// complete at the same time conflict, so the second one sees the first.
func (t *Tasks) dependents(ctx context.Context, done string) ([]string, []store.Change, error) {
	all, err := t.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	state := map[string]TaskState{}
	for _, task := range all {
		state[task.ID] = task.State
	}
	state[done] = TaskCompleted
	readied := []string{}
	var changes []store.Change
	for _, task := range all {
		if task.State != TaskWaiting || !slices.Contains(task.Needs, done) {
			continue
		}
		if allCompleted(task.Needs, state) {
			task.State = TaskReady
			readied = append(readied, task.ID)
		}
		changes = append(changes, mustChange(task))
	}
	return readied, changes, nil
}

func allCompleted(needs []string, state map[string]TaskState) bool {
	for _, id := range needs {
		if state[id] != TaskCompleted {
			return false
		}
	}
	return true
}

// release drops the claim.
func (task *Task) release() {
	task.Owner = ""
	task.token = ""
	task.LeaseUntil = time.Time{}
}

// maxRetries bounds retry. Every conflict means another operation committed,
// so the bound is only reached under extreme contention.
const maxRetries = 100

// retry runs op again while it fails with a store conflict: someone changed
// a record op read. op re-reads and re-checks everything on each run.
func retry(op func() error) error {
	var err error
	for range maxRetries {
		if err = op(); !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("core: gave up after %d conflicting attempts: %w", maxRetries, err)
}

// commit appends one task event together with the task's new projection,
// conditional on the version the task was read at (0 for a new task).
func (t *Tasks) commit(ctx context.Context, actor, typ string, task Task, payload map[string]any, more ...store.Change) error {
	var raw json.RawMessage
	if payload != nil {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	_, err := t.store.Append(ctx, store.Event{
		Type: typ, At: t.opts.Clock(), Actor: actor,
		Project: task.Project, Ticket: task.Ticket, Task: task.ID, Payload: raw,
	}, append([]store.Change{mustChange(task)}, more...)...)
	return err
}

// mustChange is the store change that writes task, conditional on the version
// it was read at. Task always marshals, so an error is a programming bug.
func mustChange(task Task) store.Change {
	data, err := json.Marshal(taskJSON{Task: task, Token: task.token})
	if err != nil {
		panic(fmt.Sprintf("core: marshal task %s: %v", task.ID, err))
	}
	return store.Change{Kind: taskKind, ID: task.ID, Data: data, IfVersion: task.version}
}

func decodeTask(rec store.Record) (Task, error) {
	var stored taskJSON
	if err := json.Unmarshal(rec.Data, &stored); err != nil {
		return Task{}, fmt.Errorf("core: task %s: %w", rec.ID, err)
	}
	task := stored.Task
	task.token = stored.Token
	task.version = rec.Version
	return task, nil
}

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}
