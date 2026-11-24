package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/store"
)

// Event types of runs (spec section 8: run.*). The transcript itself is in a
// file; the log refers to it (spec section 10).
const (
	EventRunStarted   = "run.started"
	EventRunTool      = "run.tool"
	EventRunUsage     = "run.usage"
	EventRunCompleted = "run.completed" // the agent said istari complete
	EventRunExited    = "run.exited"
)

const (
	runKind      = "run"
	runTokenKind = "run_token"
)

// Run states.
const (
	RunRunning = "running"
	RunEnded   = "ended"
)

// Codes of run token refusals.
const (
	CodeTokenInvalid = "token_invalid" // unknown, or its run ended or its lease expired
	CodeTokenScope   = "token_scope"   // the request is about another ticket or task
)

// Run is one attempt to execute a task by one agent (spec section 2).
type Run struct {
	ID         string        `json:"id"`
	Task       string        `json:"task"`
	Ticket     string        `json:"ticket"`
	Project    string        `json:"project,omitempty"`
	Role       string        `json:"role"`
	Profile    string        `json:"profile"`
	Harness    string        `json:"harness"`
	Dir        string        `json:"dir"`
	Transcript string        `json:"transcript"`
	State      string        `json:"state"`
	Started    time.Time     `json:"started"`
	Ended      time.Time     `json:"ended,omitzero"`
	Timeout    time.Duration `json:"timeout"`
	BudgetUSD  float64       `json:"budget_usd,omitempty"` // 0: none
	Usage      harness.Usage `json:"usage"`
	Completed  string        `json:"completed,omitempty"` // what the agent said with istari complete
	ExitCode   int           `json:"exit_code"`
	Reason     string        `json:"reason,omitempty"` // how the run ended

	version int64
}

// Agent is the run's actor.
func (r Run) Agent() string { return "agent:" + r.Profile }

// Runs keeps the record of runs and resolves run tokens.
type Runs struct {
	store store.Store
	tasks *core.Tasks
	clock func() time.Time
}

// NewRuns returns the runs on top of a store; tasks tells whose lease is
// current.
func NewRuns(s store.Store, tasks *core.Tasks, clock func() time.Time) *Runs {
	if clock == nil {
		clock = time.Now
	}
	return &Runs{store: s, tasks: tasks, clock: clock}
}

// Get returns one run.
func (r *Runs) Get(ctx context.Context, id string) (Run, error) {
	rec, err := r.store.Get(ctx, runKind, id)
	if errors.Is(err, store.ErrNotFound) {
		return Run{}, &core.Error{Code: core.CodeNotFound, Message: "run " + id + " does not exist"}
	}
	if err != nil {
		return Run{}, err
	}
	return decodeRun(rec)
}

// List returns every run ordered by id.
func (r *Runs) List(ctx context.Context) ([]Run, error) {
	recs, err := r.store.List(ctx, runKind)
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(recs))
	for _, rec := range recs {
		run, err := decodeRun(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// ByToken returns the run a token belongs to, while the token is valid: the
// run is running and its agent holds the task's lease (spec section 7.2: the
// token lives as long as the lease).
func (r *Runs) ByToken(ctx context.Context, token string) (Run, error) {
	invalid := &core.Error{Code: CodeTokenInvalid, Message: "the run token is unknown or no longer valid"}
	rec, err := r.store.Get(ctx, runTokenKind, tokenHash(token))
	if errors.Is(err, store.ErrNotFound) {
		return Run{}, invalid
	}
	if err != nil {
		return Run{}, err
	}
	var ref struct {
		Run string `json:"run"`
	}
	if err := json.Unmarshal(rec.Data, &ref); err != nil {
		return Run{}, err
	}
	run, err := r.Get(ctx, ref.Run)
	if err != nil {
		return Run{}, err
	}
	if run.State != RunRunning {
		return Run{}, invalid
	}
	task, err := r.tasks.Get(ctx, run.Task)
	if err != nil {
		return Run{}, err
	}
	if task.State != core.TaskClaimed || task.Owner != run.Agent() || !r.clock().Before(task.LeaseUntil) {
		return Run{}, invalid
	}
	return run, nil
}

// Complete records that the agent of a running run said its work is done
// (istari complete). The runner completes the task when the process exits.
func (r *Runs) Complete(ctx context.Context, id, message string) (Run, error) {
	if message == "" {
		message = "done"
	}
	run, err := r.Get(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if run.State != RunRunning {
		return Run{}, &core.Error{Code: core.CodeInvalidState, Message: "run " + id + " has ended"}
	}
	return r.update(ctx, id, EventRunCompleted, map[string]any{"run": id, "message": message}, func(run *Run) { run.Completed = message })
}

// create records a new run and its token.
func (r *Runs) create(ctx context.Context, run *Run) (token string, err error) {
	token = randomHex(32)
	data, _ := json.Marshal(map[string]string{"run": run.ID})
	run.State, run.Started = RunRunning, r.clock().UTC()
	return token, r.commit(ctx, run, EventRunStarted, run.Agent(),
		map[string]any{"run": run.ID, "task": run.Task, "role": run.Role, "profile": run.Profile, "harness": run.Harness, "dir": run.Dir, "transcript": run.Transcript},
		store.Change{Kind: runTokenKind, ID: tokenHash(token), Data: data})
}

// update reloads the run, applies change and records it with an event.
func (r *Runs) update(ctx context.Context, id, typ string, payload map[string]any, change func(*Run)) (Run, error) {
	for {
		run, err := r.Get(ctx, id)
		if err != nil {
			return Run{}, err
		}
		change(&run)
		err = r.commit(ctx, &run, typ, run.Agent(), payload)
		if !errors.Is(err, store.ErrConflict) {
			return run, err
		}
	}
}

func (r *Runs) commit(ctx context.Context, run *Run, typ, actor string, payload map[string]any, more ...store.Change) error {
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(payload)
	changes := append([]store.Change{{Kind: runKind, ID: run.ID, Data: data, IfVersion: run.version}}, more...)
	_, err = r.store.Append(ctx, store.Event{Type: typ, At: r.clock().UTC(), Actor: actor, Project: run.Project, Ticket: run.Ticket, Payload: raw}, changes...)
	if err == nil {
		run.version++
	}
	return err
}

func decodeRun(rec store.Record) (Run, error) {
	var run Run
	if err := json.Unmarshal(rec.Data, &run); err != nil {
		return Run{}, fmt.Errorf("run %s: %w", rec.ID, err)
	}
	run.version = rec.Version
	return run, nil
}

// tokenHash is what istari keeps of a token: the token itself is only in the
// agent's environment.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
