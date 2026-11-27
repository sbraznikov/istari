package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/skill"
)

// Defaults of the runner.
const (
	DefaultRunTimeout     = time.Hour
	DefaultStopCheckpoint = 15 * time.Minute // the limit of a stopping run's checkpoint
	DefaultPoll           = 2 * time.Second
	DefaultTranscriptMax  = 10 << 20 // bytes of a transcript file before it is rotated
	DefaultStartRetry     = 5 * time.Second
	DefaultStartPause     = time.Minute
	startFailuresToPause  = 3 // start failures in a row that pause a slot
	stopGrace             = 10 * time.Second
)

// Slot is one agent slot: a profile name, its role and how it runs. The
// profile name is the slot, not a personality (spec section 7.2).
type Slot struct {
	Profile string // e.g. implementer-2
	Role    string
	Harness harness.Profile
}

// RunnerOptions configures a Runner.
type RunnerOptions struct {
	DataDir  string
	Slots    []Slot
	Projects []string      // the projects whose tasks the slots take
	Lease    time.Duration // the task lease; renewed at a third of it. 0: core.DefaultLease
	Timeout  time.Duration // per run; 0: DefaultRunTimeout
	// Budgets are the ticket budgets by size ([defaults.budget_usd], spec
	// section 4.7). A run is capped at what is left of its ticket's budget;
	// exceeding the cap blocks the task as budget_exceeded (spec section
	// 7.5). A ticket without a size has the M budget. Empty: no caps.
	Budgets map[string]float64
	// Guard is the command the harness runs before every shell command of
	// an agent (istari hook git-guard, spec section 7.7); "" means none.
	Guard         string
	Poll          time.Duration // how often a free slot looks for work; 0: DefaultPoll
	Socket        string        // the daemon's socket, for the agent's istari commands
	TranscriptMax int64         // 0: DefaultTranscriptMax
	Clock         func() time.Time
	// Tickets gives the ticket's text for the brief, and the refiner's
	// duplicate candidates; nil: the title only.
	Tickets interface {
		Get(ctx context.Context, ref tickets.Ref) (tickets.Ticket, error)
		List(ctx context.Context, q tickets.Query) ([]tickets.Ticket, error)
	}
	// SelfReview tells whether a repository's implementer reviews its own
	// work (pipeline self_review); nil: it does.
	SelfReview func(ctx context.Context, repo string) bool
	// Checkpoints, when set, saves the work of a run stopped on its timeout,
	// its budget or by a human (spec section 7.2, step 6).
	Checkpoints *Checkpoints
	// StopCheckpoint limits that checkpoint ([daemon]
	// stop_checkpoint_timeout, spec section 4.2); 0: DefaultStopCheckpoint.
	StopCheckpoint time.Duration
	// Journal records what the runner notices about a run, such as a merge
	// of the main branch given up; nil: istari's log only.
	Journal *journal.Journal
	// Skill is the agent skill installed into every workspace as
	// .istari/skill.md when the repository has none (spec section 7.4);
	// "": istari's own.
	Skill string
	// StartRetry is how long a slot holds a task whose harness did not start
	// before it releases it, so a binary being replaced has time to come
	// back (spec section 7.3); 0: DefaultStartRetry.
	StartRetry time.Duration
	// StartPause is how long a slot claims nothing after its harness did not
	// start three times in a row (spec section 7.3); 0: DefaultStartPause.
	StartPause time.Duration
}

// Runner starts agents (spec section 7.2). Only the runner does: a free slot
// claims the ready task of its role with the highest priority and only then
// starts the harness, so the model never chooses its work.
type Runner struct {
	store   store.Store
	tasks   *core.Tasks
	tickets *core.Tickets
	ws      *Workspaces
	runs    *Runs
	opts    RunnerOptions

	mu     sync.Mutex
	active map[string]chan string  // run id → who asks it to stop
	paused map[string]PausedSlot   // slot profile → its pause
	limits map[string]harnessLimit // harness name → its usage-limit pause
}

// New returns a runner.
func New(s store.Store, tasks *core.Tasks, tickets *core.Tickets, ws *Workspaces, opts RunnerOptions) *Runner {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Lease <= 0 {
		opts.Lease = core.DefaultLease
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultRunTimeout
	}
	if opts.StopCheckpoint <= 0 {
		opts.StopCheckpoint = DefaultStopCheckpoint
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultPoll
	}
	if opts.TranscriptMax <= 0 {
		opts.TranscriptMax = DefaultTranscriptMax
	}
	if opts.StartRetry <= 0 {
		opts.StartRetry = DefaultStartRetry
	}
	if opts.StartPause <= 0 {
		opts.StartPause = DefaultStartPause
	}
	if opts.Skill == "" {
		opts.Skill = skill.Skill()
	}
	if opts.Journal == nil {
		opts.Journal = journal.New(nil, s, opts.Clock)
	}
	return &Runner{store: s, tasks: tasks, tickets: tickets, ws: ws, runs: NewRuns(s, tasks, opts.Clock), opts: opts, active: map[string]chan string{}, paused: map[string]PausedSlot{}, limits: map[string]harnessLimit{}}
}

// mainNotMerged records in the ticket's journal that the run starts without
// the main branch merged, and why, so the worker knows its branch may be
// behind (spec section 4.6, resuming: step 2).
func (r *Runner) mainNotMerged(ctx context.Context, task core.Task, cause error) {
	log.Printf("istari: run of %s: merging the main branch: %v", task.Ticket, cause)
	ref, err := tickets.ParseRef(task.Ticket)
	if err == nil {
		err = r.opts.Journal.Append(ctx, ref, journal.Entry{Kind: journal.Note, Actor: "system", ID: task.ID,
			Text: fmt.Sprintf("The main branch was not merged into the branch at the start of this run, so the branch may be behind main: %v", cause)})
	}
	if err != nil {
		log.Printf("istari: run of %s: journaling the unmerged main branch: %v", task.Ticket, err)
	}
}

// mergeConflicted records in the ticket's journal the files where merging
// the main branch at the start of the run met conflicts, once per task: a
// run started again finds the same ones (spec sections 4.6 and 4.8).
func (r *Runner) mergeConflicted(ctx context.Context, task core.Task, files []string) {
	text := "Merging the main branch into the branch met conflicts in: " + strings.Join(files, ", ")
	ref, err := tickets.ParseRef(task.Ticket)
	if err == nil {
		var entries []journal.Entry
		if entries, err = r.opts.Journal.Stored(ctx, ref); err == nil && !slices.ContainsFunc(entries, func(e journal.Entry) bool {
			return e.Kind == journal.Note && e.ID == task.ID && e.Text == text
		}) {
			err = r.opts.Journal.Append(ctx, ref, journal.Entry{Kind: journal.Note, Actor: "system", ID: task.ID, Text: text})
		}
	}
	if err != nil {
		log.Printf("istari: run of %s: journaling the merge conflicts: %v", task.Ticket, err)
	}
}

// SetSelfReview sets how the runner learns a repository's self_review.
func (r *Runner) SetSelfReview(f func(ctx context.Context, repo string) bool) { r.opts.SelfReview = f }

// Runs returns the record of runs.
func (r *Runner) Runs() *Runs { return r.runs }

// holdLease renews a lease in the background while no harness runs: while a
// run is prepared, and while it stops. started stops that and renews it once
// more, for the run to start or its task to end with; abandon only stops it.
func (r *Runner) holdLease(ctx context.Context, lease core.Lease) (started func() (core.Lease, error), abandon func()) {
	stop, done := make(chan struct{}), make(chan struct{})
	current := lease // written by the goroutine; read after done
	go func() {
		defer close(done)
		tick := time.NewTicker(r.opts.Lease / 3)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if l, err := r.tasks.Renew(ctx, current); err == nil {
					current = l
				}
			}
		}
	}()
	var once sync.Once
	halt := func() { once.Do(func() { close(stop); <-done }) }
	return func() (core.Lease, error) {
		halt()
		return r.tasks.Renew(ctx, current)
	}, halt
}

// blockBudget is the block kind of a task whose ticket's budget is spent.
const blockBudget = "budget_exceeded"

// budgetLeft is what is left of the ticket's budget: the budget of its size,
// or of the ticket whose budget it shares (full), less what the runs of every
// ticket sharing it spent. capped is false when no budgets are configured.
func (r *Runner) budgetLeft(ctx context.Context, tk core.Ticket) (left, full float64, capped bool, err error) {
	if len(r.opts.Budgets) == 0 {
		return 0, 0, false, nil
	}
	all, err := r.tickets.List(ctx)
	if err != nil {
		return 0, 0, true, err
	}
	byRef := map[string]core.Ticket{}
	for _, t := range all {
		byRef[t.Ref] = t
	}
	// The budget of a run started at a time is the first ticket's of a chain
	// of budget_of. The chain stops at a child of a split, and at a filed
	// ticket a human had admitted by then: each has a budget of its own
	// (spec section 4.5).
	root := func(t core.Ticket, at time.Time) core.Ticket {
		for range len(all) + 1 {
			parent, ok := byRef[t.BudgetOf]
			if t.BudgetOf == "" || !ok || slices.Contains(parent.Children, t.Ref) {
				break
			}
			if own := t.OwnBudgetSince; !own.IsZero() && !own.After(at) {
				break
			}
			t = parent
		}
		return t
	}
	if fresh, ok := byRef[tk.Ref]; ok {
		tk = fresh // its own budget may have started since tk was read
	} else {
		byRef[tk.Ref] = tk
	}
	owner := root(tk, r.opts.Clock())
	// A ticket that shares a budget has the budget of the one it shares
	// (spec section 4.5: a ticket filed during the work shares its origin's).
	size := owner.Size
	if size == "" {
		size = "M"
	}
	runs, err := r.runs.List(ctx)
	if err != nil {
		return 0, 0, true, err
	}
	full = r.opts.Budgets[size]
	left = full
	for _, run := range runs {
		if t, ok := byRef[run.Ticket]; ok && root(t, run.Started).Ref == owner.Ref {
			left -= run.Usage.CostUSD
		}
	}
	return left, full, true, nil
}

// budgetWaiverKind is the record of a human's istari ticket unhold --anyway:
// one more run of the ticket starts with its budget spent (spec section 7.5).
const budgetWaiverKind = "budget_waiver"

// EventBudgetWaived records that a human let one more run of a ticket start
// past its spent budget.
const EventBudgetWaived = "ticket.budget_waived"

// budgetWaiver is the record of an unhold --anyway, by ticket. It is live
// until a run uses it; Run names that run.
type budgetWaiver struct {
	Ticket string    `json:"ticket"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Run    string    `json:"run,omitempty"`
}

// waiveBudget records a live waiver for the ticket, in place of any earlier
// one, so it survives a daemon restart.
func (r *Runner) waiveBudget(ctx context.Context, actor string, tk core.Ticket) error {
	var version int64
	switch rec, err := r.store.Get(ctx, budgetWaiverKind, tk.Ref); {
	case err == nil:
		version = rec.Version
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	w := budgetWaiver{Ticket: tk.Ref, By: actor, At: r.opts.Clock().UTC()}
	// Marshalling a struct of strings and a time, and a map of strings,
	// cannot fail.
	data, _ := json.Marshal(w)
	raw, _ := json.Marshal(map[string]any{"ticket": tk.Ref, "actor": actor})
	_, err := r.store.Append(ctx, store.Event{Type: EventBudgetWaived, At: w.At, Actor: actor, Project: tk.Project, Ticket: tk.Ref, Payload: raw},
		store.Change{Kind: budgetWaiverKind, ID: tk.Ref, Data: data, IfVersion: version})
	return err
}

// useBudgetWaiver returns the change that marks the ticket's live waiver as
// used by the run, to commit with the run's start; ok is false when the
// ticket has no live waiver. The version check lets only one run use it.
func (r *Runner) useBudgetWaiver(ctx context.Context, ref, run string) (use store.Change, ok bool, err error) {
	rec, err := r.store.Get(ctx, budgetWaiverKind, ref)
	if errors.Is(err, store.ErrNotFound) {
		return store.Change{}, false, nil
	}
	if err != nil {
		return store.Change{}, false, err
	}
	var w budgetWaiver
	if err := json.Unmarshal(rec.Data, &w); err != nil {
		return store.Change{}, false, fmt.Errorf("budget waiver of %s: %w", ref, err)
	}
	if w.Run != "" {
		return store.Change{}, false, nil
	}
	w.Run = run
	data, _ := json.Marshal(w) // a struct of strings and a time cannot fail to marshal

	return store.Change{Kind: budgetWaiverKind, ID: ref, Data: data, IfVersion: rec.Version}, true, nil
}

// holdLiftedBy says what lifts each hold but budget_exceeded (spec section
// 4.1), for a refusal that names the way out.
var holdLiftedBy = map[string]string{
	"needs_input":        "an answer to every open question lifts it: a trusted member's comment on the ticket or istari ticket answer",
	"attempts_exhausted": "istari ticket retry lifts it",
	"paused":             "istari ticket resume lifts it",
	"waiting_dependency": "istari lifts it when every need is done",
	"waiting_zone":       "istari lifts it when the overlapping ticket is merged or canceled",
}

// LiftBudgetHold is a human's decision on an exceeded budget (spec section
// 7.5, istari ticket unhold): it unblocks the ticket's tasks blocked
// budget_exceeded and then clears the hold, so an interrupted call leaves the
// ticket held and a repeat finishes it. anyway lets one more run of the
// ticket start with its budget spent, capped at the budget of its size.
func (r *Runner) LiftBudgetHold(ctx context.Context, actor, ref string, anyway bool) (core.Ticket, error) {
	tk, err := r.tickets.Get(ctx, ref)
	if err != nil {
		return core.Ticket{}, err
	}
	if tk.Hold != blockBudget {
		msg := fmt.Sprintf("ticket %s is not on hold for %s", ref, blockBudget)
		if tk.Hold != "" {
			msg = fmt.Sprintf("ticket %s is on hold for %s, not %s: %s", ref, tk.Hold, blockBudget, holdLiftedBy[tk.Hold])
		}
		return core.Ticket{}, &core.Error{Code: core.CodeInvalidState, Message: msg}
	}
	if anyway {
		if err := r.waiveBudget(ctx, actor, tk); err != nil {
			return core.Ticket{}, err
		}
	} else {
		left, _, capped, err := r.budgetLeft(ctx, tk)
		if err != nil {
			return core.Ticket{}, err
		}
		if capped && left <= 0 {
			return core.Ticket{}, &core.Error{Code: core.CodeInvalidState, Message: fmt.Sprintf("the budget of %s is still spent ($%.2f left): raise its size label first, or pass --anyway", ref, left)}
		}
	}
	all, err := r.tasks.List(ctx)
	if err != nil {
		return core.Ticket{}, err
	}
	for _, task := range all {
		if task.Ticket != ref || task.State != core.TaskBlocked || task.BlockKind != blockBudget {
			continue
		}
		if err := r.tasks.Unblock(ctx, actor, task.ID); err != nil {
			return core.Ticket{}, err
		}
	}
	return r.tickets.ClearHold(ctx, actor, ref, blockBudget)
}

// Working tells which of this runner's live runs works on a task, if any.
func (r *Runner) Working(ctx context.Context, task string) (run string, ok bool) {
	runs, err := r.runs.List(ctx)
	if err != nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range runs {
		if _, live := r.active[run.ID]; live && run.Task == task {
			return run.ID, true
		}
	}
	return "", false
}

// Stop asks a running run to stop; its task fails with the reason.
func (r *Runner) Stop(ctx context.Context, id, actor string) error {
	r.mu.Lock()
	stop, ok := r.active[id]
	r.mu.Unlock()
	if !ok {
		run, err := r.runs.Get(ctx, id)
		if err != nil {
			return err
		}
		if run.State == RunEnded {
			return &core.Error{Code: core.CodeInvalidState, Message: "run " + id + " has ended: " + run.Reason}
		}
		return &core.Error{Code: core.CodeInvalidState, Message: "run " + id + " is not running here"}
	}
	select {
	case stop <- actor:
	default: // already asked
	}
	return nil
}

// Run runs every slot until ctx ends. Then the running harnesses are stopped
// and their tasks released, so another runner can take them at once.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, slot := range r.opts.Slots {
		wg.Add(1)
		go func() { defer wg.Done(); r.slot(ctx, slot) }()
	}
	wg.Wait()
}

func (r *Runner) slot(ctx context.Context, slot Slot) {
	failures := 0 // start failures in a row
	for ctx.Err() == nil {
		// While its harness's account is at its usage limit, the slot claims
		// nothing (spec section 7.3).
		if l, ok := r.limited(slot.Harness.Harness); ok {
			sleep(ctx, l.until.Sub(r.opts.Clock()))
			continue
		}
		lease, err := r.tasks.ClaimReadyIn(ctx, "agent:"+slot.Profile, slot.Role, r.opts.Projects)
		if err == nil {
			startErr := r.execute(ctx, slot, lease)
			if startErr == nil {
				failures = 0
				continue
			}
			if failures++; failures >= startFailuresToPause {
				failures = 0
				r.pause(ctx, slot, startErr)
			}
			continue
		}
		var e *core.Error
		if !errors.As(err, &e) && ctx.Err() == nil {
			log.Printf("istari: runner %s: %v", slot.Profile, err)
		}
		sleep(ctx, r.opts.Poll)
	}
}

// sleep waits for d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// PausedSlot is a slot that claims nothing for a while because its harness
// did not start three times in a row, or its harness's account is at its
// usage limit (spec section 7.3).
type PausedSlot struct {
	Slot  string    `json:"slot"`
	Error string    `json:"error"` // why the slot is paused
	Until time.Time `json:"until"` // when the slot claims again
}

// Paused returns the slots that are paused now, by profile.
func (r *Runner) Paused() []PausedSlot {
	now := r.opts.Clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []PausedSlot
	for _, p := range r.paused {
		out = append(out, p)
	}
	for _, slot := range r.opts.Slots {
		l, ok := r.limits[slot.Harness.Harness]
		if _, started := r.paused[slot.Profile]; !ok || started || !l.until.After(now) {
			continue
		}
		out = append(out, PausedSlot{Slot: slot.Profile, Error: "the usage limit of harness " + slot.Harness.Harness + ": " + l.message, Until: l.until})
	}
	slices.SortFunc(out, func(a, b PausedSlot) int { return strings.Compare(a.Slot, b.Slot) })
	return out
}

// pause keeps a slot from claiming for StartPause, and says so.
func (r *Runner) pause(ctx context.Context, slot Slot, startErr error) {
	p := PausedSlot{Slot: slot.Profile, Error: "the harness did not start: " + startErr.Error(), Until: r.opts.Clock().Add(r.opts.StartPause)}
	log.Printf("istari: runner %s: paused for %s: %s", slot.Profile, shortDuration(r.opts.StartPause), p.Error)
	r.mu.Lock()
	r.paused[slot.Profile] = p
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.paused, slot.Profile); r.mu.Unlock() }()
	sleep(ctx, r.opts.StartPause)
}

// shortDuration writes a whole number of minutes as "1m", not "1m0s".
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	return s
}

// ending is why a run ended, as the runner saw it.
type ending struct {
	reason   string
	outcome  func(context.Context, core.Lease) error // what happens to the task; nil: decide by the exit
	released bool
	save     bool // checkpoint the work before the task ends
}

// execute runs one claimed task to its end. It returns why the harness did
// not start, if it did not; any other ending is settled with the task.
func (r *Runner) execute(ctx context.Context, slot Slot, lease core.Lease) error {
	// The task's fate is settled even when ctx ends meanwhile.
	bg := context.WithoutCancel(ctx)
	fail := func(reason string) {
		if err := r.tasks.Fail(bg, lease, reason); err != nil {
			log.Printf("istari: runner %s: fail %s: %v", slot.Profile, lease.Task, err)
		}
	}
	// The lease is kept while the run is prepared (the worktree, a fetch, a
	// merge), however long that takes.
	started, abandon := r.holdLease(bg, lease)
	defer abandon()
	task, err := r.tasks.Get(bg, lease.Task)
	if err != nil {
		fail(err.Error())
		return nil
	}
	tk, err := r.tickets.Get(bg, task.Ticket)
	if err != nil {
		tk = core.Ticket{Ref: task.Ticket, Project: task.Project}
	}
	h, err := harness.Get(slot.Harness.Harness)
	if err != nil {
		fail(err.Error())
		return nil
	}
	run := Run{
		ID: "r_" + randomHex(12), Task: task.ID, Ticket: task.Ticket, Project: task.Project, Role: slot.Role,
		Profile: slot.Profile, Harness: h.Name(), Timeout: r.opts.Timeout,
	}
	budget, full, capped, err := r.budgetLeft(bg, tk)
	if err != nil {
		fail("budget: " + err.Error())
		return nil
	}
	// A human's unhold --anyway lets the next run start with the budget
	// spent, capped at the budget of the ticket's size; that run uses it up
	// either way (spec section 7.5).
	var waived []store.Change
	if capped {
		use, ok, err := r.useBudgetWaiver(bg, tk.Ref, run.ID)
		if err != nil {
			fail("budget: " + err.Error())
			return nil
		}
		if ok {
			waived = []store.Change{use}
		}
		switch {
		case budget > 0:
		case ok:
			budget = full
		default:
			if err := r.blockForBudget(tk.Ref)(bg, lease); err != nil {
				log.Printf("istari: runner %s: block %s: %v", slot.Profile, lease.Task, err)
			}
			return nil
		}
	}
	run.BudgetUSD = budget
	var conflicts []string
	if slot.Role == "refiner" {
		// The refiner works before the ticket has a worktree and reads the
		// main branch (spec section 7.2).
		run.Dir = filepath.Join(r.opts.DataDir, "runs", run.ID, "main")
		remove, err := r.ws.Scratch(bg, task.Ticket, run.Dir)
		if err != nil {
			fail("workspace: " + err.Error())
			return nil
		}
		defer remove()
	} else {
		ws, err := r.ws.Ensure(bg, tk)
		if err != nil {
			fail("workspace: " + err.Error())
			return nil
		}
		run.Dir = ws.Dir
		// The implementer's run pulls the main branch in; conflicts are its
		// first step (spec section 4.6). Other roles judge the branch as
		// the pull request has it.
		if slot.Role == "implementer" {
			if conflicts, err = r.ws.MergeMain(bg, task.Ticket); err != nil {
				r.mainNotMerged(bg, task, err)
			} else if len(conflicts) > 0 {
				r.mergeConflicted(bg, task, conflicts)
			}
		}
	}
	run.Transcript = filepath.Join(r.opts.DataDir, "runs", run.ID+".jsonl")
	token, err := r.runs.create(bg, &run, waived...)
	if err != nil {
		fail("run: " + err.Error())
		return nil
	}
	env := map[string]string{
		"ISTARI_PROFILE": slot.Profile, "ISTARI_ROLE": slot.Role, "ISTARI_TICKET": task.Ticket,
		"ISTARI_TASK": task.ID, "ISTARI_TOKEN": token, "ISTARI_RUN": run.ID, "ISTARI_WORKTREE": run.Dir,
	}
	if r.opts.Socket != "" {
		env["ISTARI_SOCKET"] = r.opts.Socket
	}
	// The agent's git and gh see none of the user's settings or logins:
	// pushing goes through istari, and the hook is not the only barrier
	// (spec sections 7.7 and 11). Commits are the run's own.
	ghDir := filepath.Join(r.opts.DataDir, "runs", run.ID, "gh")
	if err := os.MkdirAll(ghDir, 0o700); err != nil {
		fail("run: " + err.Error())
		return nil
	}
	// The roles that submit a structured result may write it to the run's
	// result file, and no other file (spec sections 7.3 and 7.6).
	if slices.Contains(resultRoles, slot.Role) {
		env["ISTARI_RESULT"] = filepath.Join(r.opts.DataDir, "runs", run.ID, "result.yaml")
	}
	who := slot.Profile + " (istari)"
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "GH_CONFIG_DIR": ghDir,
		"GIT_AUTHOR_NAME": who, "GIT_AUTHOR_EMAIL": "istari@localhost", "GIT_COMMITTER_NAME": who, "GIT_COMMITTER_EMAIL": "istari@localhost",
	} {
		env[k] = v
	}
	// The instruction comes from the main branch: the ticket's own branch
	// must not rewrite the instructions of the roles that check it.
	var mainFile skill.ReadFile
	if ref, err := tickets.ParseRef(task.Ticket); err == nil {
		mainFile = func(path string) ([]byte, error) { return r.ws.MainFile(bg, ref.Repo, path) }
	}
	instructions, err := skill.Instruction(slot.Role, mainFile)
	if err != nil {
		r.end(bg, run, harness.Result{ExitCode: -1}, harness.Usage{}, err.Error())
		fail(err.Error())
		return nil
	}
	if err := installSkill(bg, run.Dir, r.opts.Skill); err != nil {
		log.Printf("istari: run %s: installing the skill: %v", run.ID, err)
	}
	spec := slot.Harness.Apply(harness.RunSpec{Workdir: run.Dir, Instructions: instructions, Env: env, MaxBudgetUSD: run.BudgetUSD, Guard: r.opts.Guard})
	if result, ok := env["ISTARI_RESULT"]; ok {
		// "//" starts an absolute path in a rule; Edit's rule covers every
		// file tool, Write's the tool by name.
		spec.AllowedTools = append(spec.AllowedTools, "Write(/"+result+")", "Edit(/"+result+")")
	}
	if slot.Role == "implementer" {
		// The implementer may run the repository's checks itself (spec 7.3):
		// one test while it writes it, not only a whole checkpoint.
		if ref, err := tickets.ParseRef(task.Ticket); err == nil {
			checks, err := r.ws.checks(bg, ref.Repo)
			if err != nil {
				log.Printf("istari: run %s: reading the checks: %v", run.ID, err)
			}
			spec.AllowedTools = append(spec.AllowedTools, checkTools(checks)...)
		}
	}
	// The brief lists the commands the harness allows (spec 7.3), from the
	// same list, so the two cannot drift apart.
	spec.Brief = r.brief(bg, task, tk, run.Dir, conflicts, spec.AllowedTools)
	// The run starts with a whole lease, or not at all.
	renewed, err := started()
	if err != nil {
		log.Printf("istari: runner %s: task %s: the lease ran out while the run was prepared: %v", slot.Profile, lease.Task, err)
		r.end(bg, run, harness.Result{ExitCode: -1}, harness.Usage{}, "the lease ran out while the run was prepared")
		return nil
	}
	lease = renewed
	// The run is live before its harness starts: the agent may call back at
	// once, and Working and Stop must already know the run.
	stop := make(chan string, 1)
	r.mu.Lock()
	r.active[run.ID] = stop
	r.mu.Unlock()
	gone := func() { r.mu.Lock(); delete(r.active, run.ID); r.mu.Unlock() }
	defer gone()
	sess, err := h.Start(bg, spec)
	if err != nil {
		gone()
		// No agent ran: the task goes back without spending an attempt, after
		// a wait for a binary being replaced to come back (spec section 4.2).
		r.end(bg, run, harness.Result{ExitCode: -1}, harness.Usage{}, "the harness did not start: "+err.Error())
		sleep(ctx, r.opts.StartRetry)
		if rerr := r.tasks.Release(bg, lease); rerr != nil {
			log.Printf("istari: runner %s: release %s: %v", slot.Profile, lease.Task, rerr)
		}
		return err
	}

	over := make(chan struct{}, 1) // the run budget is spent
	var seen streamed
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		seen = r.stream(bg, run, sess.Events(), over)
	}()
	waited := make(chan harness.Result, 1)
	go func() {
		res, err := sess.Wait()
		if err != nil {
			res.ExitCode, res.Reason = -1, err.Error()
		}
		waited <- res
	}()

	renew := time.NewTicker(r.opts.Lease / 3)
	defer renew.Stop()
	timeout := time.NewTimer(r.opts.Timeout)
	defer timeout.Stop()
	var end *ending
	stopWith := func(e ending) {
		if end == nil {
			end = &e
			if err := sess.Stop(stopGrace); err != nil {
				log.Printf("istari: run %s: stop: %v", run.ID, err)
			}
		}
	}
	var res harness.Result
	done := ctx.Done()
loop:
	for {
		select {
		case res = <-waited:
			break loop
		case <-renew.C:
			current, err := r.tickets.Get(bg, task.Ticket)
			switch {
			case err == nil && current.State.Finished():
				// The ticket ended and canceled its tasks with it (spec 4.1):
				// the run stops, its work is saved, and the task stays
				// canceled.
				stopWith(ending{reason: "the ticket is " + string(current.State), outcome: func(context.Context, core.Lease) error { return nil }, save: true})
				continue
			case err == nil && current.Hold != "":
				// The ticket was put on hold (paused): the run stops, its work
				// is saved, and the task waits until the hold is lifted.
				// The lease is still renewed while the agent stops.
				stopWith(ending{reason: "the ticket is on hold (" + current.Hold + ")", outcome: r.tasks.Release, released: true, save: true})
			}
			l, err := r.tasks.Renew(bg, lease)
			var e *core.Error
			switch {
			case err == nil:
				lease = l
			case errors.As(err, &e):
				// The claim is gone (expired, taken): the work is not this
				// run's any more.
				stopWith(ending{reason: "the lease was lost: " + err.Error(), outcome: func(context.Context, core.Lease) error { return nil }})
			default:
				log.Printf("istari: run %s: renewing the lease: %v; trying again", run.ID, err)
			}
		case <-timeout.C:
			reason := fmt.Sprintf("timeout after %s", r.opts.Timeout)
			stopWith(ending{reason: reason, outcome: r.failWith(reason), save: true})
		case <-over:
			reason := fmt.Sprintf("the run budget of $%.2f is spent", run.BudgetUSD)
			stopWith(ending{reason: reason, outcome: r.blockForBudget(tk.Ref), save: true})
		case who := <-stop:
			reason := "stopped by " + who
			stopWith(ending{reason: reason, outcome: r.failWith(reason), save: true})
		case <-done:
			done = nil // once: then wait for the process
			stopWith(ending{reason: "the runner stopped", outcome: r.tasks.Release, released: true})
		}
	}
	<-streamDone
	if end == nil && res.BudgetSpent {
		// The harness ended the run at its own cap, the run budget: the same
		// end as istari's own budget stop (spec sections 7.2 and 7.5).
		end = &ending{reason: fmt.Sprintf("the harness spent the run budget of $%.2f", run.BudgetUSD), outcome: r.blockForBudget(tk.Ref), save: true}
	}

	// A stopping run's task stays this run's until its checkpoint and its
	// end are recorded: a stop checkpoint may outlast the lease (spec
	// section 4.2).
	stopping := end != nil && end.save
	ended := func() (core.Lease, error) { return lease, nil }
	if stopping {
		held, unheld := r.holdLease(bg, lease)
		defer unheld()
		ended = held
	}
	if stopping && r.opts.Checkpoints != nil && slot.Role == "implementer" {
		// Unfinished work is pushed as is: only what came after this is lost.
		if _, err := r.opts.Checkpoints.MakeWithin(bg, r.opts.StopCheckpoint, run, end.reason+"; continue from here"); err != nil {
			log.Printf("istari: run %s: the checkpoint on stopping: %v", run.ID, err)
		}
	}
	var idleLimit *harness.Event
	if seen.idle {
		idleLimit = seen.limit
	}
	reason, outcome, completed := r.outcome(bg, run.ID, res, idleLimit)
	if end != nil && (!end.released || !completed) {
		// A runner that stops while the agent finishes its work keeps the
		// work: a completed run completes its task even then.
		reason, outcome = end.reason, end.outcome
	}
	r.end(bg, run, res, seen.usage, reason)
	if held, err := ended(); err == nil {
		lease = held
	}
	// The harness pauses before the task goes back, so that no slot of the
	// same harness takes it at once.
	if seen.limit != nil {
		r.limit(bg, run, *seen.limit)
	} else {
		r.unlimit(run.Harness)
	}
	if err := outcome(bg, lease); err != nil {
		log.Printf("istari: runner %s: task %s: %v", slot.Profile, lease.Task, err)
	}
	return nil
}

// outcome decides by the exit and istari complete (spec section 7.3): the
// task completes only when the agent said so and the process exited cleanly.
// limit is the usage limit the harness hit before the agent worked; nil: none.
func (r *Runner) outcome(ctx context.Context, id string, res harness.Result, limit *harness.Event) (reason string, outcome func(context.Context, core.Lease) error, completed bool) {
	run, err := r.runs.Get(ctx, id)
	switch {
	case err != nil:
		return err.Error(), r.failWith(err.Error()), false
	case run.Rejected != "":
		return "rejected: " + run.Rejected, r.failWith("rejected: " + run.Rejected), false
	case run.Yielded != "":
		// Refused for the ticket's state, not the agent's work: no attempt
		// is spent (spec section 4.2).
		return "yielded: " + run.Yielded, r.tasks.Release, false
	case limit != nil && run.Completed == "":
		// The harness's account, not the agent, ended the run before any
		// work: no attempt is spent (spec section 4.2).
		return "the harness hit its usage limit: " + limit.Text, r.tasks.Release, false
	case run.Completed != "" && res.ExitCode == 0 && !res.Failed:
		return "completed: " + run.Completed, r.tasks.Complete, true
	case run.Completed == "":
		reason := fmt.Sprintf("exited with code %d without istari complete", res.ExitCode)
		return reason, r.failWith(reason), false
	default:
		reason := fmt.Sprintf("exited with code %d", res.ExitCode)
		return reason, r.failWith(reason), false
	}
}

func (r *Runner) failWith(reason string) func(context.Context, core.Lease) error {
	return func(ctx context.Context, l core.Lease) error { return r.tasks.Fail(ctx, l, reason) }
}

// blockForBudget blocks the task as budget_exceeded and holds its ticket for
// the same reason, so the block shows on the ticket (spec sections 4.1, 7.5).
// A hold that cannot be set is logged; the block stands.
func (r *Runner) blockForBudget(ticket string) func(context.Context, core.Lease) error {
	return func(ctx context.Context, l core.Lease) error {
		if err := r.tasks.Block(ctx, l, blockBudget); err != nil {
			return err
		}
		if _, err := r.tickets.SetHold(ctx, "system", ticket, blockBudget); err != nil {
			log.Printf("istari: runner: hold %s: %v", ticket, err)
		}
		return nil
	}
}

// streamed is what the runner saw of a run's events.
type streamed struct {
	usage harness.Usage
	limit *harness.Event // the first usage limit the harness hit; nil: none
	// idle reports whether the agent had called no tool when the harness hit
	// the limit: the run did no work.
	idle bool
}

// stream writes the harness's events to the transcript and the log, and
// returns what it saw. It signals over when the run budget is spent.
func (r *Runner) stream(ctx context.Context, run Run, events <-chan harness.Event, over chan<- struct{}) streamed {
	t := &transcript{path: run.Transcript, max: r.opts.TranscriptMax}
	defer func() {
		if err := t.close(); err != nil {
			log.Printf("istari: run %s: transcript: %v", run.ID, err)
		}
	}()
	var total harness.Usage
	var seen streamed
	worked := false
	for e := range events {
		if err := t.write(e); err != nil {
			log.Printf("istari: run %s: transcript: %v", run.ID, err)
		}
		switch e.Kind {
		case harness.EventUsageLimit:
			if seen.limit == nil {
				seen.limit, seen.idle = &e, !worked
			}
		case harness.EventToolCall:
			worked = true
			r.event(ctx, run, EventRunTool, map[string]any{"run": run.ID, "tool": e.Tool})
		case harness.EventUsage:
			total = total.Add(e.Usage)
			r.event(ctx, run, EventRunUsage, map[string]any{"run": run.ID, "usage": e.Usage, "total_usd": total.CostUSD})
			if run.BudgetUSD > 0 && total.CostUSD > run.BudgetUSD {
				select {
				case over <- struct{}{}:
				default:
				}
			}
		}
	}
	seen.usage = total
	return seen
}

func (r *Runner) event(ctx context.Context, run Run, typ string, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	if _, err := r.store.Append(ctx, store.Event{Type: typ, At: r.opts.Clock().UTC(), Actor: run.Agent(), Project: run.Project, Ticket: run.Ticket, Payload: raw}); err != nil {
		log.Printf("istari: run %s: %v", run.ID, err)
	}
}

// end records how the run ended. The harness's own total, when it reports
// one, is the run's cost, even below the sum of its usage events, which may
// be an estimate (spec section 7.5).
func (r *Runner) end(ctx context.Context, run Run, res harness.Result, usage harness.Usage, reason string) {
	if res.Usage.CostUSD > 0 {
		usage = res.Usage
	}
	if _, err := r.runs.end(ctx, run.ID, reason, &runExit{code: res.ExitCode, usage: usage}); err != nil {
		log.Printf("istari: run %s: %v", run.ID, err)
	}
}

// brief assembles the run's brief from the ticket and its journal.
func (r *Runner) brief(ctx context.Context, task core.Task, tk core.Ticket, dir string, conflicts, tools []string) string {
	in := BriefInput{Role: task.Role, Task: task.ID, Ref: tk.Ref, Title: tk.Title, SelfReview: true, Conflicts: conflicts, AllowedTools: tools}
	ref, err := tickets.ParseRef(tk.Ref)
	if err != nil {
		return Brief(in)
	}
	if r.opts.SelfReview != nil {
		in.SelfReview = r.opts.SelfReview(ctx, ref.Repo)
	}
	if r.opts.Tickets != nil {
		if t, err := r.opts.Tickets.Get(ctx, ref); err == nil {
			in.Title, in.Body = t.Title, t.Body
			if task.Role == "refiner" {
				cands, err := tickets.Candidates(ctx, r.opts.Tickets, t)
				if err != nil {
					log.Printf("istari: brief of %s: candidates: %v", tk.Ref, err)
				}
				for _, c := range cands {
					in.Candidates = append(in.Candidates, fmt.Sprintf("%s %s (shares %s)", c.Ref, c.Title, strings.Join(c.Shared, ", ")))
				}
			}
		} else {
			log.Printf("istari: brief of %s: the ticket text: %v", tk.Ref, err)
		}
	}
	if in.Journal, err = journal.New(nil, r.store, nil).Stored(ctx, ref); err != nil {
		log.Printf("istari: brief of %s: the journal: %v", tk.Ref, err)
	}
	if task.Role == "reviewer" && r.ws != nil {
		var zone []string
		for _, e := range in.Journal {
			if e.Kind == journal.Plan {
				zone = e.Plan.Zone // the latest plan's
			}
		}
		if review, err := r.ws.ReviewInput(ctx, tk.Ref, dir, zone); err != nil {
			log.Printf("istari: brief of %s: the review: %v", tk.Ref, err)
		} else {
			in.Review = &review
		}
	}
	return Brief(in)
}

// installSkill writes the agent skill into the workspace when the
// repository has none, and keeps git from seeing it as a change.
func installSkill(ctx context.Context, dir, skill string) error {
	path := filepath.Join(dir, ".istari", "skill.md")
	if skill == "" {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(skill), 0o644); err != nil {
		return err
	}
	exclude, err := gitOut(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(exclude, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString("\n/.istari/skill.md\n")
	return errors.Join(err, f.Close())
}

// resultRoles are the roles that submit a structured result and get a result
// file for it, ISTARI_RESULT (spec section 7.6).
var resultRoles = []string{"refiner", "planner", "reviewer"}

// checkTools are the tool rules that let an agent run the check commands:
// each command's program and subcommand with any arguments, so that
// "go test ./... -short" allows "Bash(go test *)". The leading words end at
// the first argument, a word that starts with "-", ".", "/" or "{" (the
// placeholders {files} and {dirs}). Sorted, one
// rule per program and subcommand.
func checkTools(checks pipeline.Checks) []string {
	var rules []string
	for _, c := range checks.List {
		var lead []string
		for _, w := range strings.Fields(c.Run) {
			if strings.ContainsAny(w[:1], "-./{") {
				break
			}
			lead = append(lead, w)
		}
		if len(lead) == 0 {
			continue
		}
		rules = append(rules, "Bash("+strings.Join(lead, " ")+" *)")
	}
	slices.Sort(rules)
	return slices.Compact(rules)
}
