package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Files reads a repository's files on its default branch.
type Files interface {
	ReadFile(ctx context.Context, repo, path string) ([]byte, error)
}

// EngineOptions configures an Engine.
type EngineOptions struct {
	Defaults config.Defaults
	Files    Files // nil: every repository has the default pipeline
	Actor    string
	// CacheFor is how long a repository's pipeline file is kept; 0: a minute.
	CacheFor time.Duration
	Clock    func() time.Time
}

// Engine moves tickets through their pipeline (spec section 4.3): it starts
// a ready ticket (T3) and expands its pipeline into tasks with needs, and
// fires the ticket's transitions as the tasks of the stages complete.
type Engine struct {
	tasks   *core.Tasks
	tickets *core.Tickets
	opts    EngineOptions

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	p  Pipeline
	at time.Time
}

// NewEngine returns an engine.
func NewEngine(tasks *core.Tasks, tk *core.Tickets, opts EngineOptions) *Engine {
	if opts.Actor == "" {
		opts.Actor = "system"
	}
	if opts.CacheFor <= 0 {
		opts.CacheFor = time.Minute
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Engine{tasks: tasks, tickets: tk, opts: opts, cache: map[string]cached{}}
}

// Load returns the repository's pipeline: its file, validated, or the default.
func (e *Engine) Load(ctx context.Context, repo string) (Pipeline, error) {
	e.mu.Lock()
	c, ok := e.cache[repo]
	e.mu.Unlock()
	if ok && e.opts.Clock().Sub(c.at) < e.opts.CacheFor {
		return c.p, nil
	}
	p := Default()
	if e.opts.Files != nil {
		data, err := e.opts.Files.ReadFile(ctx, repo, Path)
		switch {
		case err == nil:
			if p, err = Parse(data); err != nil {
				return Pipeline{}, fmt.Errorf("%s: %w", repo, err)
			}
		case !errors.Is(err, tickets.ErrNotFound):
			return Pipeline{}, err
		}
	}
	if err := p.Validate(); err != nil {
		return Pipeline{}, fmt.Errorf("%s: %w", repo, err)
	}
	e.mu.Lock()
	e.cache[repo] = cached{p: p, at: e.opts.Clock()}
	e.mu.Unlock()
	return p, nil
}

// taskID names a stage's task of a ticket in a round, so expanding twice
// creates nothing new: s_<ticket hash>_r<round>_<stage>.
func taskID(ref string, round int, stage string) string {
	return fmt.Sprintf("%s_r%d_%s", ticketPrefix(ref), round, stage)
}

func ticketPrefix(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "s_" + hex.EncodeToString(sum[:])[:10]
}

// StageOf returns the pipeline stage of a task the engine made, or "".
func StageOf(task core.Task) string {
	_, stage := parseID(task.ID)
	return stage
}

// RoundOf returns the review round of a task the engine made, or 0.
func RoundOf(task core.Task) int {
	round, _ := parseID(task.ID)
	return round
}

func parseID(id string) (round int, stage string) {
	rest, ok := strings.CutPrefix(id, "s_")
	if !ok || len(rest) < 11 || rest[10] != '_' {
		return 0, ""
	}
	r, stage, ok := strings.Cut(rest[11:], "_")
	if !ok || !strings.HasPrefix(r, "r") {
		return 0, ""
	}
	round, err := strconv.Atoi(r[1:])
	if err != nil {
		return 0, ""
	}
	return round, stage
}

// round is a ticket's tasks of its latest round, by stage.
type round struct {
	n     int
	tasks map[string]core.Task
}

func roundOf(ref string, all []core.Task) round {
	r := round{tasks: map[string]core.Task{}}
	prefix := ticketPrefix(ref) + "_"
	for _, t := range all {
		if t.Ticket != ref || !strings.HasPrefix(t.ID, prefix) {
			continue
		}
		n, stage := parseID(t.ID)
		switch {
		case n > r.n:
			r.n, r.tasks = n, map[string]core.Task{stage: t}
		case n == r.n:
			r.tasks[stage] = t
		}
	}
	return r
}

// done tells whether every one of the stages that the round has is completed.
func (r round) done(stages ...string) bool {
	for _, s := range stages {
		if t, ok := r.tasks[s]; ok && t.State != core.TaskCompleted {
			return false
		}
	}
	return true
}

// Reconcile moves every ticket as far as its tasks allow. It is idempotent.
func (e *Engine) Reconcile(ctx context.Context, all []core.Ticket) error {
	tasks, err := e.tasks.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, tk := range all {
		if err := e.reconcile(ctx, tk, tasks); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tk.Ref, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) reconcile(ctx context.Context, tk core.Ticket, all []core.Task) error {
	if tk.Hold != "" {
		return nil
	}
	r := roundOf(tk.Ref, all)
	fire := func(trigger string) error {
		_, err := e.tickets.Fire(ctx, e.opts.Actor, tk.Ref, trigger)
		return err
	}
	switch tk.State {
	case core.TicketReady:
		// T3. Claims are what the slots and limits gate; starting gives the
		// ticket its worktree and its planner task (spec section 4.1).
		if err := fire("start"); err != nil {
			return err
		}
		tk.State = core.TicketPlanning
		return e.reconcile(ctx, tk, all)
	case core.TicketPlanning:
		if r.n == 0 {
			if err := e.expand(ctx, tk, 1, nil); err != nil {
				return err
			}
			tasks, err := e.tasks.List(ctx)
			if err != nil {
				return err
			}
			r = roundOf(tk.Ref, tasks)
		}
		if r.done("plan", PlanApproval) {
			return fire("plan_accepted") // T4
		}
	case core.TicketInReview:
		if r.done("review") {
			return fire("review_passed") // T8
		}
	case core.TicketAwaitingApproval:
		if r.done(StageMerge) {
			return fire("merged") // T11
		}
	}
	return nil
}

// expand creates the tasks of a round; from, if set, keeps only those
// stages (a new review round starts at implement).
func (e *Engine) expand(ctx context.Context, tk core.Ticket, n int, from func(TaskSpec) bool) error {
	ref, err := tickets.ParseRef(tk.Ref)
	if err != nil {
		return err
	}
	p, err := e.Load(ctx, ref.Repo)
	if err != nil {
		return err
	}
	s := p.Settings(e.opts.Defaults)
	specs := p.Expand(s)
	kept := map[string]bool{}
	for _, spec := range specs {
		if from == nil || from(spec) {
			kept[spec.Stage] = true
		}
	}
	for _, spec := range specs {
		if !kept[spec.Stage] {
			continue
		}
		var needs []string
		for _, need := range spec.Needs {
			if kept[need] {
				needs = append(needs, taskID(tk.Ref, n, need))
			}
		}
		_, err := e.tasks.Create(ctx, e.opts.Actor, core.NewTask{
			ID: taskID(tk.Ref, n, spec.Stage), Ticket: tk.Ref, Project: tk.Project, Role: spec.Role, Needs: needs, MaxAttempts: s.MaxAttempts,
		})
		var ce *core.Error
		if errors.As(err, &ce) && ce.Code == core.CodeInvalidTask && strings.Contains(ce.Message, "already exists") {
			continue // an earlier reconcile made it
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// RequestChanges starts the next review round (spec section 4.3): the open
// tasks of the round end, and a new round runs from implement on. After
// max_review_rounds rounds the ticket is held instead.
func (e *Engine) RequestChanges(ctx context.Context, refText, actor string) error {
	tk, err := e.tickets.Get(ctx, refText)
	if err != nil {
		return err
	}
	ref, err := tickets.ParseRef(refText)
	if err != nil {
		return err
	}
	p, err := e.Load(ctx, ref.Repo)
	if err != nil {
		return err
	}
	s := p.Settings(e.opts.Defaults)
	all, err := e.tasks.List(ctx)
	if err != nil {
		return err
	}
	r := roundOf(refText, all)
	if s.MaxReviewRounds > 0 && r.n >= s.MaxReviewRounds {
		if _, err := e.tickets.SetHold(ctx, actor, refText, "attempts_exhausted"); err != nil {
			return err
		}
		return &core.Error{Code: core.CodeLimitReached, Message: fmt.Sprintf("%s has had %d review rounds, the max_review_rounds of its pipeline", refText, r.n)}
	}
	for _, t := range r.tasks {
		if !slices.Contains([]core.TaskState{core.TaskCompleted, core.TaskFailed, core.TaskCanceled}, t.State) {
			if err := e.tasks.Cancel(ctx, actor, t.ID); err != nil {
				return err
			}
		}
	}
	started := false
	return e.expand(ctx, tk, r.n+1, func(spec TaskSpec) bool {
		started = started || spec.Stage == "implement"
		return started
	})
}
