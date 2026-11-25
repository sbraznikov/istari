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
	"github.com/sbraznikov/istari/internal/tickets/journal"
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
	// NeedsRefine tells whether a new ticket falls short of the ticket
	// standard and needs the refiner (spec section 4.5); nil: no refining.
	NeedsRefine func(ctx context.Context, tk core.Ticket) (bool, error)
	// HasPlan tells whether a plan was submitted for the ticket (T4 needs
	// "the plan is published in the ticket"); nil: the plan task's
	// completion is enough.
	HasPlan func(ctx context.Context, ref string) (bool, error)
	// Checks tells the CI state of a ticket's pull request (pending, success
	// or failure); ok is false while it has none. nil: CI is not watched.
	Checks func(ctx context.Context, ref string) (state string, ok bool)
	// Ready marks a ticket's draft pull request ready for review; it is
	// called on green CI before T6 and must succeed for T6 to fire. nil: the
	// pull request is left as it is.
	Ready func(ctx context.Context, ref string) error
	// Review tells how many findings code accepted in the reviewer's report
	// of a review task; submitted is false while it has none (spec 7.6:
	// the verdict is code's). nil: the review task's completion passes.
	Review func(ctx context.Context, ref, task string) (findings int, submitted bool, err error)
	// Merged tells whether the PR watch saw the ticket's pull request merged:
	// a human may merge it before istari's own merge stage, and the ticket
	// then finishes through T11 (spec 4.8). nil: only istari merges.
	Merged func(ctx context.Context, ref string) (bool, error)
	// Journal is where the engine notes what it does not act on: a ticket
	// that got open needs after its pull request was opened (spec 4.1).
	// nil: nothing is noted.
	Journal *journal.Journal
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

	states map[string]core.TicketState // of this reconcile's tickets
}

type cached struct {
	p   Pipeline
	err error
	at  time.Time
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
		return c.p, c.err
	}
	p, err := e.read(ctx, repo)
	var fetch *fetchError
	if errors.As(err, &fetch) {
		return Pipeline{}, err // the tracker failed: ask again next time
	}
	e.mu.Lock()
	e.cache[repo] = cached{p: p, err: err, at: e.opts.Clock()}
	e.mu.Unlock()
	return p, err
}

type fetchError struct{ err error }

func (f *fetchError) Error() string { return f.err.Error() }
func (f *fetchError) Unwrap() error { return f.err }

// read reads and validates a repository's pipeline.
func (e *Engine) read(ctx context.Context, repo string) (Pipeline, error) {
	p := Default()
	if e.opts.Files != nil {
		data, err := e.opts.Files.ReadFile(ctx, repo, Path)
		switch {
		case err == nil:
			if p, err = Parse(data); err != nil {
				return Pipeline{}, fmt.Errorf("%s: %w", repo, err)
			}
		case !errors.Is(err, tickets.ErrNotFound):
			return Pipeline{}, &fetchError{err}
		}
	}
	if err := p.Validate(); err != nil {
		return Pipeline{}, fmt.Errorf("%s: %w", repo, err)
	}
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

// round is a ticket's tasks of its latest round, by stage; others counts its
// tasks that the engine did not make.
type round struct {
	n      int
	tasks  map[string]core.Task
	others int
}

func roundOf(ref string, all []core.Task) round {
	r := round{tasks: map[string]core.Task{}}
	prefix := ticketPrefix(ref) + "_"
	for _, t := range all {
		if t.Ticket != ref {
			continue
		}
		if !strings.HasPrefix(t.ID, prefix) {
			r.others++
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

// done tells whether the engine's round exists and every one of the stages
// it has is completed. A ticket without the engine's tasks is not moved on.
func (r round) done(stages ...string) bool {
	if r.n == 0 {
		return false
	}
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
	e.states = map[string]core.TicketState{}
	for _, tk := range all {
		e.states[tk.Ref] = tk.State
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
	if beforeReview(tk.State) && (tk.Hold == "" || tk.Hold == waitingDependency) {
		// T3 needs the tickets in needs completed, and so does the work
		// after it when needs appear later (spec 4.1). A ticket that waits
		// for them says so with the hold waiting_dependency, which goes when
		// they are done; a need that never will be (canceled) leaves the hold
		// for a human to change the needs.
		waiting := e.needOpen(ctx, tk)
		switch {
		case waiting && tk.Hold == "":
			_, err := e.tickets.SetHold(ctx, e.opts.Actor, tk.Ref, waitingDependency)
			return err
		case waiting:
			return nil
		case tk.Hold == waitingDependency && !e.childCanceled(ctx, tk):
			// A canceled child holds its parent the same way; that hold
			// waits for a human.
			if _, err := e.tickets.ClearHold(ctx, e.opts.Actor, tk.Ref, waitingDependency); err != nil {
				return err
			}
			tk.Hold = ""
		}
	}
	if (tk.State == core.TicketInReview || tk.State == core.TicketAwaitingApproval) && e.needOpen(ctx, tk) {
		// Its pull request is open: istari does not stop it, a human decides.
		if err := e.noteOnce(ctx, tk.Ref, lateNeeds); err != nil {
			return err
		}
	}
	if tk.Hold != "" {
		return nil
	}
	if tk.State != core.TicketDone && tk.State != core.TicketCanceled && exhausted(tk.Ref, all) {
		_, err := e.tickets.SetHold(ctx, e.opts.Actor, tk.Ref, "attempts_exhausted")
		return err
	}
	r := roundOf(tk.Ref, all)
	if from := resumeFrom(tk.State); from != nil && r.n == 0 && r.others == 0 && len(tk.Children) == 0 {
		// A ticket past planning without tasks was recovered from the
		// tracker (istari recover): it gets the stages of its state on.
		if err := e.expand(ctx, tk, 1, from); err != nil {
			return err
		}
		tasks, err := e.tasks.List(ctx)
		if err != nil {
			return err
		}
		r = roundOf(tk.Ref, tasks)
	}
	fire := func(trigger string) error {
		_, err := e.tickets.Fire(ctx, e.opts.Actor, tk.Ref, trigger)
		return err
	}
	switch tk.State {
	case core.TicketInProgress, core.TicketInReview, core.TicketAwaitingApproval:
		if e.opts.Merged == nil {
			break
		}
		merged, err := e.opts.Merged(ctx, tk.Ref)
		if err != nil {
			return err
		}
		if merged {
			return fire("merged") // T11: a human merged first
		}
	}
	switch tk.State {
	case core.TicketNew:
		// A ticket below the standard gets a refine task; the refiner brings
		// it to the standard or asks. While it still falls short, the check is
		// repeated (after an answer, an edit or a failed run, spec 4.5) up to
		// maxRefines times; then it waits for a human.
		if e.opts.NeedsRefine == nil {
			return nil
		}
		done := 0
		for _, t := range all {
			if t.Ticket != tk.Ref || !strings.HasPrefix(StageOf(t), "refine-") {
				continue
			}
			if t.State != core.TaskCompleted && t.State != core.TaskFailed && t.State != core.TaskCanceled {
				return nil // a refine is under way
			}
			done++
		}
		needs, err := e.opts.NeedsRefine(ctx, tk)
		if err != nil || !needs {
			return err
		}
		if done >= maxRefines {
			_, err := e.tickets.SetHold(ctx, e.opts.Actor, tk.Ref, "attempts_exhausted")
			return err
		}
		_, err = e.tasks.Create(ctx, e.opts.Actor, core.NewTask{ID: taskID(tk.Ref, 0, fmt.Sprintf("refine-%d", done+1)), Ticket: tk.Ref, Project: tk.Project, Role: "refiner"})
		return err
	case core.TicketReady:
		// T3. Claims are what the slots and limits gate; starting gives the
		// ticket its worktree and its planner task (spec section 4.1). A
		// ticket whose pipeline is invalid does not start.
		ref, err := tickets.ParseRef(tk.Ref)
		if err != nil {
			return err
		}
		if _, err := e.Load(ctx, ref.Repo); err != nil {
			return err
		}
		if err := fire("start"); err != nil {
			return err
		}
		tk.State = core.TicketPlanning
		return e.reconcile(ctx, tk, all)
	case core.TicketPlanning:
		if r.n == 0 && r.others > 0 {
			return nil // someone made this ticket's tasks by hand: they are theirs
		}
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
			// T4 needs a published plan, and a ticket above max_size reaches
			// in_progress only by a split (T5). Otherwise a human looks.
			planned := true
			if e.opts.HasPlan != nil {
				var err error
				if planned, err = e.opts.HasPlan(ctx, tk.Ref); err != nil {
					return err
				}
			}
			over, err := e.tooLarge(ctx, tk)
			if err != nil {
				return err
			}
			if !planned || over {
				_, err := e.tickets.SetHold(ctx, e.opts.Actor, tk.Ref, "needs_input")
				return err
			}
			return fire("plan_accepted") // T4
		}
	case core.TicketInProgress:
		if e.opts.Checks != nil && len(tk.Children) == 0 {
			if state, ok := e.opts.Checks(ctx, tk.Ref); ok {
				return e.verify(ctx, tk, r, state)
			}
		}
		if len(tk.Children) > 0 {
			for _, c := range tk.Children {
				switch e.state(ctx, c) {
				case core.TicketDone:
				case core.TicketCanceled:
					// T12 needs every child done: a human decides what now.
					_, err := e.tickets.SetHold(ctx, e.opts.Actor, tk.Ref, waitingDependency)
					return err
				default:
					return nil
				}
			}
			return fire("children_done") // T12
		}
	case core.TicketInReview:
		if !r.done("review") {
			return nil
		}
		if e.opts.Review == nil {
			return fire("review_passed") // T8
		}
		return e.verdict(ctx, tk, r, fire)
	case core.TicketAwaitingApproval:
		if r.done(StageMerge) {
			return fire("merged") // T11
		}
	}
	return nil
}

const waitingDependency = "waiting_dependency"

// lateNeeds is the journal note on a ticket that got open needs after its
// pull request was opened.
const lateNeeds = "this ticket got open needs after its pull request was opened; it is not held waiting_dependency (spec 4.1): a human decides whether it waits"

// needOpen tells whether one of the ticket's needs is not done.
func (e *Engine) needOpen(ctx context.Context, tk core.Ticket) bool {
	return slices.ContainsFunc(tk.Needs, func(n string) bool { return e.state(ctx, n) != core.TicketDone })
}

// noteOnce adds a note to the ticket's journal unless it has it already, so
// the engine's reconciles stay idempotent.
func (e *Engine) noteOnce(ctx context.Context, ref, text string) error {
	if e.opts.Journal == nil {
		return nil
	}
	r, err := tickets.ParseRef(ref)
	if err != nil {
		return err
	}
	entries, err := e.opts.Journal.Stored(ctx, r)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(entries, func(en journal.Entry) bool { return en.Kind == journal.Note && en.Text == text }) {
		return nil
	}
	return e.opts.Journal.Append(ctx, r, journal.Entry{Kind: journal.Note, Actor: e.opts.Actor, Text: text})
}

// childCanceled tells whether one of the ticket's children is canceled.
func (e *Engine) childCanceled(ctx context.Context, tk core.Ticket) bool {
	return slices.ContainsFunc(tk.Children, func(c string) bool { return e.state(ctx, c) == core.TicketCanceled })
}

// beforeReview tells whether a ticket in state has no pull request under
// review yet: an open need holds it (spec 4.1).
func beforeReview(state core.TicketState) bool {
	return state == core.TicketReady || state == core.TicketPlanning || state == core.TicketInProgress
}

// resumeFrom tells, for a ticket past planning, the stages its state still
// has ahead, in pipeline order: in_progress from the implementer's stage,
// in_review after verify, awaiting_approval after the reviewer's. nil: the
// state has no stages to resume.
func resumeFrom(state core.TicketState) func(TaskSpec) bool {
	// after starts at the first stage behind the last one that is.
	after := func(is func(TaskSpec) bool) func(TaskSpec) bool {
		seen := false
		return func(s TaskSpec) bool {
			if is(s) {
				seen = true
				return false
			}
			return seen
		}
	}
	var starts func(TaskSpec) bool
	switch state {
	case core.TicketInProgress:
		starts = func(s TaskSpec) bool { return s.Role == "implementer" }
	case core.TicketInReview:
		starts = after(func(s TaskSpec) bool { return s.Stage == StageVerify })
	case core.TicketAwaitingApproval:
		starts = after(func(s TaskSpec) bool { return s.Role == "reviewer" })
	default:
		return nil
	}
	started := false
	return func(s TaskSpec) bool {
		started = started || starts(s)
		return started
	}
}

// maxRefines bounds how often a ticket below the standard is refined.
const maxRefines = 3

// exhausted tells whether a task of the ticket has used up its attempts
// (task.attempts_exhausted, spec 4.1): it holds the ticket until a human
// retries it. Refine tasks are bounded by maxRefines instead.
func exhausted(ref string, all []core.Task) bool {
	return slices.ContainsFunc(all, func(t core.Task) bool {
		return t.Ticket == ref && t.State == core.TaskFailed && t.Attempts >= t.MaxAttempts && !strings.HasPrefix(StageOf(t), "refine-")
	})
}

// verify follows CI once the implementer is done (spec 4.3, stage verify):
// green ends the pull request's draft, completes the verify task and fires
// T6; red starts a rework round.
func (e *Engine) verify(ctx context.Context, tk core.Ticket, r round, state string) error {
	v, ok := r.tasks[StageVerify]
	if !ok || v.State != core.TaskReady { // the implementer is not done, or it was handled
		return nil
	}
	switch state {
	case "success":
		if e.opts.Ready != nil {
			// Until it is marked ready, T6 waits: the next reconcile retries.
			if err := e.opts.Ready(ctx, tk.Ref); err != nil {
				return fmt.Errorf("mark the pull request ready: %w", err)
			}
		}
		lease, err := e.tasks.Claim(ctx, e.opts.Actor, v.ID)
		if err != nil {
			return err
		}
		if err := e.tasks.Complete(ctx, lease); err != nil {
			return err
		}
		_, err = e.tickets.Fire(ctx, e.opts.Actor, tk.Ref, "pr_ready") // T6
		return err
	case "failure":
		// The red CI is handled once: the next round's verify waits for its
		// implementer, and a round that leaves CI red counts toward
		// max_review_rounds.
		err := e.RequestChanges(ctx, tk.Ref, e.opts.Actor)
		if ce := (*core.Error)(nil); errors.As(err, &ce) && ce.Code == core.CodeLimitReached {
			return nil // the ticket is held for a human
		}
		return err
	}
	return nil
}

// verdict computes the review's verdict from the accepted report (spec
// sections 4.1 and 7.6): any accepted finding returns the ticket for rework
// (T7) while rounds are left, else it is held; none passes it once CI is
// green (T8). Without a report there is no verdict: a reviewer cannot
// complete its task without one.
func (e *Engine) verdict(ctx context.Context, tk core.Ticket, r round, fire func(string) error) error {
	findings, submitted, err := e.opts.Review(ctx, tk.Ref, r.tasks["review"].ID)
	switch {
	case err != nil:
		return err
	case !submitted:
		return nil
	case findings > 0:
		err := e.RequestChanges(ctx, tk.Ref, e.opts.Actor)
		if ce := (*core.Error)(nil); errors.As(err, &ce) && ce.Code == core.CodeLimitReached {
			return nil // the ticket is held for a human
		}
		if err != nil {
			return err
		}
		return fire("changes_requested") // T7
	}
	if e.opts.Checks != nil {
		if state, ok := e.opts.Checks(ctx, tk.Ref); !ok || state != "success" {
			return nil
		}
	}
	return fire("review_passed") // T8
}

// tooLarge tells whether a ticket is larger than its pipeline's max_size.
func (e *Engine) tooLarge(ctx context.Context, tk core.Ticket) (bool, error) {
	if tk.Size == "" {
		return false, nil
	}
	ref, err := tickets.ParseRef(tk.Ref)
	if err != nil {
		return false, err
	}
	s, err := e.SettingsOf(ctx, ref.Repo)
	if err != nil {
		return false, err
	}
	return slices.Index(core.Sizes(), tk.Size) > slices.Index(core.Sizes(), s.MaxSize), nil
}

// SettingsOf returns the settings of a repository's pipeline.
func (e *Engine) SettingsOf(ctx context.Context, repo string) (Settings, error) {
	p, err := e.Load(ctx, repo)
	if err != nil {
		return Settings{}, err
	}
	return p.Settings(e.opts.Defaults), nil
}

// state returns a ticket's state, from this reconcile or the store.
func (e *Engine) state(ctx context.Context, ref string) core.TicketState {
	if st, ok := e.states[ref]; ok {
		return st
	}
	if tk, err := e.tickets.Get(ctx, ref); err == nil {
		return tk.State
	}
	return ""
}

// expand creates the tasks of a round; from, if set, keeps only those
// stages (a new review round starts at implement).
func (e *Engine) expand(ctx context.Context, tk core.Ticket, n int, from func(TaskSpec) bool, after ...string) error {
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
		if len(needs) == 0 {
			needs = append(needs, after...) // the round's first tasks wait for what came before
		}
		id := taskID(tk.Ref, n, spec.Stage)
		if _, err := e.tasks.Get(ctx, id); err == nil {
			continue // an earlier reconcile made it
		}
		if _, err := e.tasks.Create(ctx, e.opts.Actor, core.NewTask{
			ID: id, Ticket: tk.Ref, Project: tk.Project, Role: spec.Role, Needs: needs, MaxAttempts: s.MaxAttempts,
		}); err != nil {
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
	// The rework needs the work it reworks (spec 4.3): the round's
	// implement task, when it was done.
	var after []string
	if impl, ok := r.tasks["implement"]; ok && impl.State == core.TaskCompleted {
		after = []string{impl.ID}
	}
	started := false
	return e.expand(ctx, tk, r.n+1, func(spec TaskSpec) bool {
		started = started || spec.Stage == "implement"
		return started
	}, after...)
}
