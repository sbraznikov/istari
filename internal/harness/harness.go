// Package harness runs coding agents as external programs (spec section 7.3).
// A Harness starts one run without a terminal; its Session yields the run's
// events and its result. Adapters live in subpackages and register
// themselves, so the rest of istari depends only on this package.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// RunSpec is everything a harness needs to start one run.
type RunSpec struct {
	Workdir        string            // the ticket's worktree; the run starts there
	Brief          string            // the first message
	Instructions   string            // the role instruction (system prompt)
	Env            map[string]string // the agent's own variables (ISTARI_*); nothing else of the daemon's environment is passed
	Model          string
	PermissionMode string
	AllowedTools   []string
	MaxBudgetUSD   float64 // the harness-side spending cap; 0 means none
	// Guard is a command the harness runs before every shell command of the
	// agent, with the harness's hook input on stdin; exit status 2 denies
	// the command and its stderr tells the agent why (spec section 7.7:
	// the prohibitions are enforced by a harness hook). "" means none.
	Guard string
}

// EventKind is the kind of a harness event.
type EventKind string

// Event kinds.
const (
	EventMessage  EventKind = "message"   // text the agent wrote
	EventToolCall EventKind = "tool_call" // a tool the agent called
	EventUsage    EventKind = "usage"     // tokens of one model call
	// EventUsageLimit means the harness's account hit its usage limit: the
	// run cannot work until Reset (spec section 4.2). Text is the harness's
	// message.
	EventUsageLimit EventKind = "usage_limit"
)

// Event is one thing that happened in a run.
type Event struct {
	Kind      EventKind       `json:"kind"`
	Text      string          `json:"text,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Usage     Usage           `json:"usage,omitzero"`
	// Reset is when a usage limit ends, as the harness named it; zero: it
	// named no time.
	Reset time.Time `json:"reset,omitzero"`
}

// Usage counts tokens and cost.
type Usage struct {
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Add returns the sum of u and o.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
		CostUSD:          u.CostUSD + o.CostUSD,
	}
}

// Result is how a run ended.
type Result struct {
	ExitCode  int    // the process exit code; -1 if it was killed by a signal
	Usage     Usage  // the run's totals
	SessionID string // for Resume
	Failed    bool   // the harness reported that the run ended in an error
	Reason    string // how the harness says the run ended, e.g. success or error_max_budget_usd
	// BudgetSpent means the harness ended the run at its own spending cap,
	// RunSpec.MaxBudgetUSD: a budget stop, not a failure of the agent.
	BudgetSpent bool
}

// Session is one running harness process.
type Session interface {
	// ID is the harness's session id, known once the harness reports it.
	ID() string
	// Events yields the run's events in order and closes when the process's
	// output ends.
	Events() <-chan Event
	// Send passes a human's reply into the running session.
	Send(msg string) error
	// Stop asks the process to end, and kills it after grace.
	Stop(grace time.Duration) error
	// Wait waits for the process to end and returns its result.
	Wait() (Result, error)
}

// Harness starts runs of one agent program.
type Harness interface {
	// Name is the name the config uses, e.g. in a pipeline stage.
	Name() string
	Start(ctx context.Context, spec RunSpec) (Session, error)
	// Check tells whether the harness can run here (istari doctor).
	Check(ctx context.Context) error
}

// HookCall is what a guard hook is asked about: a shell command, or the file
// a file tool writes; both empty for any other tool.
type HookCall struct {
	Command string // the shell command line
	Path    string // the file a file tool writes
	Cwd     string // where the agent works
}

// HookReader is a harness that runs RunSpec.Guard as a hook: it reads its
// own hook input. The guard command it runs gets "--harness <name>"
// appended, so istari knows whose input it reads.
type HookReader interface {
	ReadHook(raw []byte) (HookCall, error)
}

// Resumer is a harness that can continue an earlier session. It speeds up
// continuation but is not required (spec section 7.3).
type Resumer interface {
	Resume(ctx context.Context, sessionID string, spec RunSpec) (Session, error)
}

// ErrSendNotSupported means the session cannot take input while it runs.
var ErrSendNotSupported = errors.New("harness: this session does not accept messages while it runs; resume it instead")

// Profile is how one role runs: which harness, model and permissions.
type Profile struct {
	Harness        string
	Model          string
	PermissionMode string
	AllowedTools   []string
}

// Apply fills the profile's settings into a run spec.
func (p Profile) Apply(spec RunSpec) RunSpec {
	spec.Model = p.Model
	spec.PermissionMode = p.PermissionMode
	spec.AllowedTools = slices.Clone(p.AllowedTools)
	return spec
}

var (
	mu        sync.Mutex
	harnesses = map[string]Harness{}
	defaults  map[string]Profile
)

// RegisterDefaults sets the roles' profiles to use until the pipeline config
// sets them. The first adapter's are kept; an adapter calls it from init.
func RegisterDefaults(profiles map[string]Profile) {
	mu.Lock()
	defer mu.Unlock()
	if defaults == nil {
		defaults = profiles
	}
}

// Defaults returns the roles' default profiles, by role.
func Defaults() map[string]Profile {
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]Profile, len(defaults))
	for role, p := range defaults {
		out[role] = p
	}
	return out
}

// Register makes a harness available by its name. Adapters call it from init.
func Register(h Harness) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := harnesses[h.Name()]; dup {
		panic(fmt.Sprintf("harness: %s registered twice", h.Name()))
	}
	harnesses[h.Name()] = h
}

// Get returns a registered harness.
func Get(name string) (Harness, error) {
	mu.Lock()
	defer mu.Unlock()
	h, ok := harnesses[name]
	if !ok {
		return nil, fmt.Errorf("harness: no harness named %q", name)
	}
	return h, nil
}

// All returns the registered harnesses ordered by name.
func All() []Harness {
	mu.Lock()
	defer mu.Unlock()
	all := make([]Harness, 0, len(harnesses))
	for _, h := range harnesses {
		all = append(all, h)
	}
	slices.SortFunc(all, func(a, b Harness) int { return strings.Compare(a.Name(), b.Name()) })
	return all
}
