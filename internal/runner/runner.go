package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/project"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// Defaults of the runner.
const (
	DefaultRunTimeout    = time.Hour
	DefaultPoll          = 2 * time.Second
	DefaultTranscriptMax = 10 << 20 // bytes of a transcript file before it is rotated
	stopGrace            = 10 * time.Second
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
	// BudgetUSD is the spending cap of one run; 0 means none. Exceeding it
	// blocks the task as budget_exceeded (spec section 7.5).
	BudgetUSD     float64
	Poll          time.Duration // how often a free slot looks for work; 0: DefaultPoll
	Socket        string        // the daemon's socket, for the agent's istari commands
	TranscriptMax int64         // 0: DefaultTranscriptMax
	Clock         func() time.Time
	// Tickets gives the ticket's text for the brief; nil: the title only.
	Tickets interface {
		Get(ctx context.Context, ref tickets.Ref) (tickets.Ticket, error)
	}
	// Skill is the agent skill installed into every workspace as
	// .istari/skill.md when the repository has none (spec section 7.4);
	// "": istari's own.
	Skill string
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
	active map[string]chan string // run id → who asks it to stop
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
	if opts.Poll <= 0 {
		opts.Poll = DefaultPoll
	}
	if opts.TranscriptMax <= 0 {
		opts.TranscriptMax = DefaultTranscriptMax
	}
	if opts.Skill == "" {
		opts.Skill = project.Skill()
	}
	return &Runner{store: s, tasks: tasks, tickets: tickets, ws: ws, runs: NewRuns(s, tasks, opts.Clock), opts: opts, active: map[string]chan string{}}
}

// Runs returns the record of runs.
func (r *Runner) Runs() *Runs { return r.runs }

// Stop asks a running run to stop; its task fails with the reason.
func (r *Runner) Stop(ctx context.Context, id, actor string) error {
	r.mu.Lock()
	stop, ok := r.active[id]
	r.mu.Unlock()
	if !ok {
		if _, err := r.runs.Get(ctx, id); err != nil {
			return err
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
	for ctx.Err() == nil {
		lease, err := r.tasks.ClaimReadyIn(ctx, "agent:"+slot.Profile, slot.Role, r.opts.Projects)
		if err == nil {
			r.execute(ctx, slot, lease)
			continue
		}
		var e *core.Error
		if !errors.As(err, &e) && ctx.Err() == nil {
			log.Printf("istari: runner %s: %v", slot.Profile, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.opts.Poll):
		}
	}
}

// ending is why a run ended, as the runner saw it.
type ending struct {
	reason   string
	outcome  func(context.Context, core.Lease) error // what happens to the task; nil: decide by the exit
	released bool
}

// execute runs one claimed task to its end.
func (r *Runner) execute(ctx context.Context, slot Slot, lease core.Lease) {
	// The task's fate is settled even when ctx ends meanwhile.
	bg := context.WithoutCancel(ctx)
	fail := func(reason string) {
		if err := r.tasks.Fail(bg, lease, reason); err != nil {
			log.Printf("istari: runner %s: fail %s: %v", slot.Profile, lease.Task, err)
		}
	}
	task, err := r.tasks.Get(bg, lease.Task)
	if err != nil {
		fail(err.Error())
		return
	}
	tk, err := r.tickets.Get(bg, task.Ticket)
	if err != nil {
		tk = core.Ticket{Ref: task.Ticket, Project: task.Project}
	}
	h, err := harness.Get(slot.Harness.Harness)
	if err != nil {
		fail(err.Error())
		return
	}
	run := Run{
		ID: "r_" + randomHex(12), Task: task.ID, Ticket: task.Ticket, Project: task.Project, Role: slot.Role,
		Profile: slot.Profile, Harness: h.Name(), Timeout: r.opts.Timeout, BudgetUSD: r.opts.BudgetUSD,
	}
	if slot.Role == "refiner" {
		// The refiner works before the ticket has a worktree and reads the
		// main branch (spec section 7.2).
		run.Dir = filepath.Join(r.opts.DataDir, "runs", run.ID, "main")
		remove, err := r.ws.Scratch(bg, task.Ticket, run.Dir)
		if err != nil {
			fail("workspace: " + err.Error())
			return
		}
		defer remove()
	} else {
		ws, err := r.ws.Ensure(bg, tk)
		if err != nil {
			fail("workspace: " + err.Error())
			return
		}
		run.Dir = ws.Dir
	}
	run.Transcript = filepath.Join(r.opts.DataDir, "runs", run.ID+".jsonl")
	token, err := r.runs.create(bg, &run)
	if err != nil {
		fail("run: " + err.Error())
		return
	}
	env := map[string]string{
		"ISTARI_PROFILE": slot.Profile, "ISTARI_ROLE": slot.Role, "ISTARI_TICKET": task.Ticket,
		"ISTARI_TASK": task.ID, "ISTARI_TOKEN": token, "ISTARI_RUN": run.ID,
	}
	if r.opts.Socket != "" {
		env["ISTARI_SOCKET"] = r.opts.Socket
	}
	instructions, err := pipeline.Instruction(slot.Role, run.Dir)
	if err != nil {
		r.end(bg, run, harness.Result{ExitCode: -1}, harness.Usage{}, err.Error())
		fail(err.Error())
		return
	}
	if err := installSkill(bg, run.Dir, r.opts.Skill); err != nil {
		log.Printf("istari: run %s: installing the skill: %v", run.ID, err)
	}
	spec := slot.Harness.Apply(harness.RunSpec{Workdir: run.Dir, Brief: r.brief(bg, task, tk), Instructions: instructions, Env: env, MaxBudgetUSD: r.opts.BudgetUSD})
	sess, err := h.Start(bg, spec)
	if err != nil {
		r.end(bg, run, harness.Result{ExitCode: -1}, harness.Usage{}, "the harness did not start: "+err.Error())
		fail("the harness did not start: " + err.Error())
		return
	}

	stop := make(chan string, 1)
	r.mu.Lock()
	r.active[run.ID] = stop
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.active, run.ID); r.mu.Unlock() }()

	over := make(chan struct{}, 1) // the run budget is spent
	var usage harness.Usage
	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		usage = r.stream(bg, run, sess.Events(), over)
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
			sess.Stop(stopGrace)
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
			stopWith(ending{reason: reason, outcome: r.failWith(reason)})
		case <-over:
			reason := fmt.Sprintf("the run budget of $%.2f is spent", r.opts.BudgetUSD)
			stopWith(ending{reason: reason, outcome: func(c context.Context, l core.Lease) error { return r.tasks.Block(c, l, "budget_exceeded") }})
		case who := <-stop:
			reason := "stopped by " + who
			stopWith(ending{reason: reason, outcome: r.failWith(reason)})
		case <-done:
			done = nil // once: then wait for the process
			stopWith(ending{reason: "the runner stopped", outcome: r.tasks.Release, released: true})
		}
	}
	<-streamed

	reason, outcome, completed := r.outcome(bg, run.ID, res)
	if end != nil && !(end.released && completed) {
		// A runner that stops while the agent finishes its work keeps the
		// work: a completed run completes its task even then.
		reason, outcome = end.reason, end.outcome
	}
	r.end(bg, run, res, usage, reason)
	if err := outcome(bg, lease); err != nil {
		log.Printf("istari: runner %s: task %s: %v", slot.Profile, lease.Task, err)
	}
}

// outcome decides by the exit and istari complete (spec section 7.3): the
// task completes only when the agent said so and the process exited cleanly.
func (r *Runner) outcome(ctx context.Context, id string, res harness.Result) (reason string, outcome func(context.Context, core.Lease) error, completed bool) {
	run, err := r.runs.Get(ctx, id)
	switch {
	case err != nil:
		return err.Error(), r.failWith(err.Error()), false
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

// stream writes the harness's events to the transcript and the log, and
// returns the run's usage. It signals over when the run budget is spent.
func (r *Runner) stream(ctx context.Context, run Run, events <-chan harness.Event, over chan<- struct{}) harness.Usage {
	t := &transcript{path: run.Transcript, max: r.opts.TranscriptMax}
	defer t.close()
	var total harness.Usage
	for e := range events {
		if err := t.write(e); err != nil {
			log.Printf("istari: run %s: transcript: %v", run.ID, err)
		}
		switch e.Kind {
		case harness.EventToolCall:
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
	return total
}

func (r *Runner) event(ctx context.Context, run Run, typ string, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	if _, err := r.store.Append(ctx, store.Event{Type: typ, At: r.opts.Clock().UTC(), Actor: run.Agent(), Project: run.Project, Ticket: run.Ticket, Payload: raw}); err != nil {
		log.Printf("istari: run %s: %v", run.ID, err)
	}
}

// end records how the run ended.
func (r *Runner) end(ctx context.Context, run Run, res harness.Result, usage harness.Usage, reason string) {
	if res.Usage.CostUSD > usage.CostUSD {
		usage = res.Usage // the harness's own total, when it reports one
	}
	_, err := r.runs.update(ctx, run.ID, EventRunExited, map[string]any{"run": run.ID, "exit_code": res.ExitCode, "reason": reason, "usage": usage},
		func(run *Run) {
			run.State, run.Ended, run.ExitCode, run.Reason, run.Usage = RunEnded, r.opts.Clock().UTC(), res.ExitCode, reason, usage
		})
	if err != nil {
		log.Printf("istari: run %s: %v", run.ID, err)
	}
}

// brief assembles the run's brief from the ticket and its journal.
func (r *Runner) brief(ctx context.Context, task core.Task, tk core.Ticket) string {
	in := BriefInput{Role: task.Role, Task: task.ID, Ref: tk.Ref, Title: tk.Title}
	ref, err := tickets.ParseRef(tk.Ref)
	if err != nil {
		return Brief(in)
	}
	if r.opts.Tickets != nil {
		if t, err := r.opts.Tickets.Get(ctx, ref); err == nil {
			in.Title, in.Body = t.Title, t.Body
		} else {
			log.Printf("istari: brief of %s: the ticket text: %v", tk.Ref, err)
		}
	}
	if in.Journal, err = journal.New(nil, r.store, nil).Stored(ctx, ref); err != nil {
		log.Printf("istari: brief of %s: the journal: %v", tk.Ref, err)
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
	defer f.Close()
	_, err = f.WriteString("\n/.istari/skill.md\n")
	return err
}
