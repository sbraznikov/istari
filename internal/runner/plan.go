package runner

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// Seam is a public boundary a test observes, agreed before the first test:
// what it catches and what it misses (spec section 7.6).
type Seam struct {
	Name    string `yaml:"name" json:"name"`
	Catches string `yaml:"catches" json:"catches"`
	Misses  string `yaml:"misses" json:"misses"`
}

// PlanResult is the planner's structured plan (spec section 4.9:
// istari plan submit): slices, seams, change zone, and a split when the
// ticket is too large.
type PlanResult struct {
	Summary    string        `yaml:"summary" json:"summary"`
	Slices     []string      `yaml:"slices" json:"slices"`
	Seams      []Seam        `yaml:"seams" json:"seams"`
	Zone       []string      `yaml:"zone" json:"zone"` // paths; "path (new)" for a file the change creates
	Size       string        `yaml:"size" json:"size,omitempty"`
	SizeReason string        `yaml:"size_reason" json:"size_reason,omitempty"`
	Split      *tickets.Plan `yaml:"split" json:"split,omitempty"`
}

// ParsePlanResult reads and validates a plan against its schema.
func ParsePlanResult(data []byte) (PlanResult, error) {
	var r PlanResult
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return PlanResult{}, yamlError("plan", data, err)
	}
	bad := func(format string, args ...any) error { return fmt.Errorf("plan: "+format, args...) }
	switch {
	case r.Summary == "":
		return PlanResult{}, bad("a summary is needed")
	case len(r.Slices) == 0:
		return PlanResult{}, bad("slices are needed: one test and the minimum code each")
	case len(r.Seams) == 0:
		return PlanResult{}, bad("seams are needed: the boundaries tests observe")
	case len(r.Zone) == 0:
		return PlanResult{}, bad("a change zone is needed: the paths the change touches")
	case r.Size != "" && !slices.Contains(core.Sizes(), r.Size):
		return PlanResult{}, bad("size %q is not one of %s", r.Size, strings.Join(core.Sizes(), ", "))
	}
	for _, s := range r.Seams {
		if s.Name == "" || s.Catches == "" || s.Misses == "" {
			return PlanResult{}, bad("seam %q needs what it catches and what it misses", s.Name)
		}
	}
	return r, nil
}

// zoneFiles is how many files of the change zone each size allows.
var zoneFiles = map[string]int{"XS": 2, "S": 5, "M": 12, "L": 25}

// PlanSettings are the pipeline's values a plan is checked against.
type PlanSettings struct {
	MaxSize string
	Gate    bool // plan_gate: a human approves the plan with its seams
}

// Plans checks and records plans; code, not the planner, does the writing.
type Plans struct {
	ws      *Workspaces
	journal *journal.Journal
	tickets *core.Tickets
	sizer   Sizer
	tasks   *core.Tasks
	creator tickets.Creator
	created func(ctx context.Context, refs []tickets.Ref, budgetOf string) error
}

// Sizer writes a ticket's size label to its tracker.
type Sizer interface {
	SetSize(ctx context.Context, ref tickets.Ref, size string) error
}

// NewPlans returns the plan checker; sizer writes the size label (nil: istari only).
func NewPlans(ws *Workspaces, j *journal.Journal, k *core.Tickets, sizer Sizer) *Plans {
	return &Plans{ws: ws, journal: j, tickets: k, sizer: sizer}
}

// Check validates a plan against the repository and the ticket (spec 4.9):
// the zone's paths exist on the main branch or are marked new; the number of
// files matches the size, else the size is revised; a ticket larger than
// max_size must be split.
func (p *Plans) Check(ctx context.Context, run Run, res PlanResult, s PlanSettings) (PlanResult, error) {
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return PlanResult{}, err
	}
	for _, entry := range res.Zone {
		path, isNew := strings.CutSuffix(strings.TrimSpace(entry), "(new)")
		path = strings.TrimSpace(path)
		if isNew {
			continue
		}
		kind, err := p.ws.MainHas(ctx, ref.Repo, path)
		if err != nil {
			return PlanResult{}, err
		}
		if kind == "tree" {
			return PlanResult{}, fmt.Errorf("plan: the zone's %s is a directory; list the files the change touches", path)
		}
		if kind == "" {
			return PlanResult{}, fmt.Errorf("plan: the zone's path %s does not exist on the main branch; mark it \"%s (new)\" if the change creates it", path, path)
		}
	}
	tk, err := p.tickets.Get(ctx, run.Ticket)
	if err != nil {
		return PlanResult{}, err
	}
	size := res.Size
	if size == "" {
		size = tk.Size
	}
	if size != "" && size != "XL" && len(res.Zone) > zoneFiles[size] {
		for _, bigger := range core.Sizes()[slices.Index(core.Sizes(), size)+1:] {
			if n, ok := zoneFiles[bigger]; !ok || len(res.Zone) <= n {
				res.SizeReason = strings.TrimSpace(fmt.Sprintf("%s the change zone has %d files, more than %s allows (%d)", res.SizeReason, len(res.Zone), size, zoneFiles[size]))
				size = bigger
				break
			}
		}
	}
	res.Size = size
	if size != "" && s.MaxSize != "" && slices.Index(core.Sizes(), size) > slices.Index(core.Sizes(), s.MaxSize) && (res.Split == nil || len(res.Split.Tickets) == 0) {
		return PlanResult{}, fmt.Errorf("plan: the ticket is %s, larger than max_size %s: it must be split into child tickets (split:)", size, s.MaxSize)
	}
	return res, nil
}

// Record writes a checked plan: the plan entry, its seams as an assumption
// when no human approves them (spec 7.6), and a revised size.
func (p *Plans) Record(ctx context.Context, run Run, res PlanResult, s PlanSettings) error {
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return err
	}
	var seams []string
	for _, sm := range res.Seams {
		seams = append(seams, fmt.Sprintf("%s — catches %s; misses %s", sm.Name, sm.Catches, sm.Misses))
	}
	plan := journal.Entry{Kind: journal.Plan, Actor: run.Agent(), Plan: &journal.PlanEntry{Summary: res.Summary, Slices: res.Slices, Seams: seams, Zone: res.Zone}}
	if err := p.journal.Append(ctx, ref, plan); err != nil {
		return err
	}
	if !s.Gate {
		e := journal.Entry{Kind: journal.Assumption, Actor: run.Agent(), Text: "Seams agreed by the planner, without a human: " + strings.Join(seams, "; ")}
		if err := p.journal.Append(ctx, ref, e); err != nil {
			return err
		}
	}
	tk, err := p.tickets.Get(ctx, run.Ticket)
	if err != nil {
		return err
	}
	if res.Size == "" || res.Size == tk.Size {
		return nil
	}
	from := tk.Size
	if from == "" {
		from = "none"
	}
	if _, err := p.tickets.SetSize(ctx, run.Agent(), run.Ticket, res.Size); err != nil {
		return err
	}
	if p.sizer != nil {
		if err := p.sizer.SetSize(ctx, ref, res.Size); err != nil {
			return err
		}
	}
	return p.journal.Append(ctx, ref, journal.Entry{Kind: journal.Assumption, Actor: run.Agent(), Text: fmt.Sprintf("Size %s → %s: %s", from, res.Size, res.SizeReason)})
}

// WithSplit lets the plans split tickets: tasks to end the parent's own,
// creator to make the children in the tracker, and created to mirror them
// into istari with the parent's admission and budget.
func (p *Plans) WithSplit(tasks *core.Tasks, creator tickets.Creator, created func(ctx context.Context, refs []tickets.Ref, budgetOf string) error) *Plans {
	p.tasks, p.creator, p.created = tasks, creator, created
	return p
}

// Split splits the run's ticket into child tickets (T5, spec sections 4.3 and
// 4.9): they are created as a plan with the ticket as their parent, admitted
// by inheritance, recorded as its children; the parent moves to in_progress
// and its own open tasks end, as a parent has none.
func (p *Plans) Split(ctx context.Context, run Run, plan tickets.Plan) ([]string, error) {
	if p.creator == nil || p.created == nil || p.tasks == nil {
		return nil, fmt.Errorf("plan: splitting needs the ticket tracker")
	}
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return nil, err
	}
	for i := range plan.Tickets {
		if plan.Tickets[i].Parent == "" {
			plan.Tickets[i].Parent = run.Ticket
		}
	}
	refs, err := tickets.CreatePlan(ctx, p.creator, plan, ref.Repo)
	if err != nil {
		return nil, err
	}
	var children []string
	var list []tickets.Ref
	for _, pt := range plan.Tickets {
		children = append(children, refs[pt.Key].String())
		list = append(list, refs[pt.Key])
	}
	if err := p.created(ctx, list, run.Ticket); err != nil {
		return nil, err
	}
	if _, err := p.tickets.SetChildren(ctx, run.Agent(), run.Ticket, children); err != nil {
		return nil, err
	}
	if _, err := p.tickets.Fire(ctx, run.Agent(), run.Ticket, "split"); err != nil {
		return nil, err
	}
	all, err := p.tasks.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if t.Ticket != run.Ticket || t.ID == run.Task || t.State == core.TaskCompleted || t.State == core.TaskFailed || t.State == core.TaskCanceled {
			continue
		}
		if err := p.tasks.Cancel(ctx, run.Agent(), t.ID); err != nil {
			return nil, err
		}
	}
	return children, nil
}
