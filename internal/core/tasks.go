package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	if opts.Lease <= 0 {
		opts.Lease = DefaultLease
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
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
// completed, and waiting otherwise.
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
	task := Task{
		ID: nt.ID, Ticket: nt.Ticket, Project: nt.Project, Role: nt.Role,
		Needs: nt.Needs, MaxAttempts: nt.MaxAttempts, CreatedAt: t.opts.Clock().UTC(),
	}
	if task.MaxAttempts <= 0 {
		task.MaxAttempts = t.opts.MaxAttempts
	}
	err := retry(func() error {
		if _, err := t.Get(ctx, task.ID); err == nil {
			return errorf(CodeInvalidTask, "task %s already exists", task.ID)
		}
		// Needs may only name existing tasks, so needs can never form a cycle
		// longer than a task naming itself.
		var guards []store.Change
		task.State = TaskReady
		for _, id := range task.Needs {
			if id == task.ID {
				return errorf(CodeInvalidNeeds, "task %s needs itself", task.ID)
			}
			need, err := t.Get(ctx, id)
			if err != nil {
				return errorf(CodeInvalidNeeds, "task %s needs %s, which does not exist", task.ID, id)
			}
			if need.State != TaskCompleted {
				task.State = TaskWaiting
			}
			// Rewriting the need unchanged makes a concurrent completion of it
			// conflict with this create, so the new task cannot miss it.
			guard, err := taskChange(need, need.version)
			if err != nil {
				return err
			}
			guards = append(guards, guard)
		}
		_, err := t.commit(ctx, actor, "task.created", task, 0, map[string]any{"state": task.State}, guards...)
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return task, nil
}

// Claim claims one ready task for actor.
func (t *Tasks) Claim(ctx context.Context, actor, id string) (Lease, error) {
	task, err := t.Get(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	return t.claim(ctx, actor, task)
}

// ClaimReady claims the oldest ready task of the role. It fails with
// no_ready_task when there is none. Priority ordering comes later (spec 4.9).
func (t *Tasks) ClaimReady(ctx context.Context, actor, role string) (Lease, error) {
	for {
		all, err := t.List(ctx)
		if err != nil {
			return Lease{}, err
		}
		var ready []Task
		for _, task := range all {
			if task.State == TaskReady && task.Role == role {
				ready = append(ready, task)
			}
		}
		if len(ready) == 0 {
			return Lease{}, errorf(CodeNoReadyTask, "no ready task for role %s", role)
		}
		slices.SortStableFunc(ready, func(a, b Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
		lost := false
		for _, task := range ready {
			lease, err := t.claim(ctx, actor, task)
			var e *Error
			if errors.As(err, &e) && e.Code == CodeTaskClaimed {
				lost = true
				continue // another worker was faster; try the next one
			}
			return lease, err
		}
		if !lost {
			return Lease{}, errorf(CodeNoReadyTask, "no ready task for role %s", role)
		}
	}
}

func (t *Tasks) claim(ctx context.Context, actor string, task Task) (Lease, error) {
	if task.State != TaskReady {
		return Lease{}, notClaimable(task)
	}
	read := task.version
	task.State = TaskClaimed
	task.Owner = actor
	task.token = randomHex(16)
	task.LeaseUntil = t.opts.Clock().UTC().Add(t.opts.Lease)
	payload := map[string]any{"lease_until": task.LeaseUntil}
	if _, err := t.commit(ctx, actor, "task.claimed", task, read, payload); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return Lease{}, errorf(CodeTaskClaimed, "task %s was claimed by someone else", task.ID)
		}
		return Lease{}, err
	}
	return Lease{Task: task.ID, Owner: actor, Token: task.token, Until: task.LeaseUntil}, nil
}

// Release gives the task back: it returns to ready without using an attempt.
func (t *Tasks) Release(ctx context.Context, lease Lease) error {
	return retry(func() error {
		task, err := t.owned(ctx, lease)
		if err != nil {
			return err
		}
		read := task.version
		task.State = TaskReady
		task.release()
		_, err = t.commit(ctx, lease.Owner, "task.released", task, read, nil)
		return err
	})
}

// BlockKinds are the reasons a task can be blocked (spec section 4.2).
var BlockKinds = []string{"needs_input", "awaiting_approval", "budget_exceeded", "ci_red"}

// Block stops work on the owner's task for a reason of one of BlockKinds.
// The claim ends; Unblock returns the task to ready.
func (t *Tasks) Block(ctx context.Context, lease Lease, kind string) error {
	if !slices.Contains(BlockKinds, kind) {
		return errorf(CodeInvalidTask, "block kind %q is not one of %v", kind, BlockKinds)
	}
	return retry(func() error {
		task, err := t.owned(ctx, lease)
		if err != nil {
			return err
		}
		read := task.version
		task.State = TaskBlocked
		task.BlockKind = kind
		task.release()
		_, err = t.commit(ctx, lease.Owner, "task.blocked", task, read, map[string]any{"kind": kind})
		return err
	})
}

// Unblock returns a blocked task to ready.
func (t *Tasks) Unblock(ctx context.Context, actor, id string) error {
	return retry(func() error {
		task, err := t.Get(ctx, id)
		if err != nil {
			return err
		}
		if task.State != TaskBlocked {
			return errorf(CodeInvalidState, "task %s is %s, not blocked", id, task.State)
		}
		read, kind := task.version, task.BlockKind
		task.State = TaskReady
		task.BlockKind = ""
		_, err = t.commit(ctx, actor, "task.unblocked", task, read, map[string]any{"kind": kind})
		return err
	})
}

// Complete marks the owner's task completed and readies every waiting task
// whose needs are now all completed, in the same event.
func (t *Tasks) Complete(ctx context.Context, lease Lease) error {
	return retry(func() error {
		task, err := t.owned(ctx, lease)
		if err != nil {
			return err
		}
		read := task.version
		task.State = TaskCompleted
		task.release()
		readied, changes, err := t.readyDependents(ctx, task.ID)
		if err != nil {
			return err
		}
		_, err = t.commit(ctx, lease.Owner, "task.completed", task, read, map[string]any{"readied": readied}, changes...)
		return err
	})
}

// Fail records a failed attempt. Below the attempt limit the task returns to
// ready (task.failed); at the limit it stays failed and the event is
// task.attempts_exhausted, which asks for the ticket hold attempts_exhausted.
func (t *Tasks) Fail(ctx context.Context, lease Lease, reason string) error {
	return retry(func() error {
		task, err := t.owned(ctx, lease)
		if err != nil {
			return err
		}
		read := task.version
		task.Attempts++
		task.release()
		typ := "task.failed"
		task.State = TaskReady
		if task.Attempts >= task.MaxAttempts {
			typ = "task.attempts_exhausted"
			task.State = TaskFailed
		}
		payload := map[string]any{"reason": reason, "attempts": task.Attempts, "max_attempts": task.MaxAttempts}
		_, err = t.commit(ctx, lease.Owner, typ, task, read, payload)
		return err
	})
}

// Cancel cancels a task from any open state.
func (t *Tasks) Cancel(ctx context.Context, actor, id string) error {
	return retry(func() error {
		task, err := t.Get(ctx, id)
		if err != nil {
			return err
		}
		switch task.State {
		case TaskWaiting, TaskReady, TaskClaimed, TaskBlocked:
		default:
			return errorf(CodeInvalidState, "task %s is %s and cannot be canceled", id, task.State)
		}
		read := task.version
		task.State = TaskCanceled
		task.release()
		_, err = t.commit(ctx, actor, "task.canceled", task, read, nil)
		return err
	})
}

// Renew extends the owner's lease by the lease duration from now.
func (t *Tasks) Renew(ctx context.Context, lease Lease) (Lease, error) {
	err := retry(func() error {
		task, err := t.owned(ctx, lease)
		if err != nil {
			return err
		}
		read := task.version
		task.LeaseUntil = t.opts.Clock().UTC().Add(t.opts.Lease)
		if _, err := t.commit(ctx, lease.Owner, "task.renewed", task, read, map[string]any{"lease_until": task.LeaseUntil}); err != nil {
			return err
		}
		lease.Until = task.LeaseUntil
		return nil
	})
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// ExpireLeases returns every claimed task whose lease deadline has passed by
// the clock to ready, with one task.expired event each, and returns their ids.
// The old owner's lease stops working. The scheduler calls it on every tick.
func (t *Tasks) ExpireLeases(ctx context.Context) ([]string, error) {
	all, err := t.List(ctx)
	if err != nil {
		return nil, err
	}
	now := t.opts.Clock()
	expired := []string{}
	for _, task := range all {
		if task.State != TaskClaimed || now.Before(task.LeaseUntil) {
			continue
		}
		owner, until, read := task.Owner, task.LeaseUntil, task.version
		task.State = TaskReady
		task.release()
		_, err := t.commit(ctx, "system", "task.expired", task, read, map[string]any{"owner": owner, "lease_until": until})
		if errors.Is(err, store.ErrConflict) {
			continue // renewed, completed or released meanwhile
		}
		if err != nil {
			return expired, err
		}
		expired = append(expired, task.ID)
	}
	return expired, nil
}

// owned returns the task the lease is for, or claim_lost if the lease is no
// longer the task's current claim.
func (t *Tasks) owned(ctx context.Context, lease Lease) (Task, error) {
	task, err := t.Get(ctx, lease.Task)
	if err != nil {
		return Task{}, err
	}
	if task.State != TaskClaimed || task.token == "" || task.token != lease.Token {
		return Task{}, errorf(CodeClaimLost, "the claim on task %s is no longer held", lease.Task)
	}
	return task, nil
}

// readyDependents returns the waiting tasks that become ready once done is
// completed, and the changes that make them ready.
func (t *Tasks) readyDependents(ctx context.Context, done string) ([]string, []store.Change, error) {
	all, err := t.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	state := map[string]TaskState{done: TaskCompleted}
	for _, task := range all {
		if task.ID != done {
			state[task.ID] = task.State
		}
	}
	readied := []string{}
	var changes []store.Change
	for _, task := range all {
		if task.State != TaskWaiting || !slices.Contains(task.Needs, done) {
			continue
		}
		if !allCompleted(task.Needs, state) {
			continue
		}
		read := task.version
		task.State = TaskReady
		change, err := taskChange(task, read)
		if err != nil {
			return nil, nil, err
		}
		readied = append(readied, task.ID)
		changes = append(changes, change)
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

func notClaimable(task Task) error {
	if task.State == TaskClaimed {
		return errorf(CodeTaskClaimed, "task %s is claimed by %s", task.ID, task.Owner)
	}
	return errorf(CodeInvalidState, "task %s is %s, not ready", task.ID, task.State)
}

// commit appends one task event together with the task's new projection,
// conditional on the version read before the change (0 for a new task).
func (t *Tasks) commit(ctx context.Context, actor, typ string, task Task, read int64, payload map[string]any, more ...store.Change) (store.Event, error) {
	change, err := taskChange(task, read)
	if err != nil {
		return store.Event{}, err
	}
	var raw json.RawMessage
	if payload != nil {
		if raw, err = json.Marshal(payload); err != nil {
			return store.Event{}, err
		}
	}
	return t.store.Append(ctx, store.Event{
		Type: typ, At: t.opts.Clock(), Actor: actor,
		Project: task.Project, Ticket: task.Ticket, Task: task.ID, Payload: raw,
	}, append([]store.Change{change}, more...)...)
}

func taskChange(task Task, read int64) (store.Change, error) {
	data, err := json.Marshal(taskJSON{Task: task, Token: task.token})
	if err != nil {
		return store.Change{}, err
	}
	return store.Change{Kind: taskKind, ID: task.ID, Data: data, IfVersion: read}, nil
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

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}
