// Package claudecode is the harness adapter for Claude Code (spec section
// 7.3): it starts `claude --print` without a terminal and reads its
// stream-json output.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sbraznikov/istari/internal/harness"
)

// Name is the harness name in the config.
const Name = "claude-code"

// Options configures the adapter.
type Options struct {
	Binary string // default "claude", looked up in PATH
}

// Harness starts Claude Code runs.
type Harness struct {
	binary string
}

var (
	_ harness.Harness = (*Harness)(nil)
	_ harness.Resumer = (*Harness)(nil)
)

func init() { harness.Register(New(Options{})) }

// New returns the adapter.
func New(opts Options) *Harness {
	if opts.Binary == "" {
		opts.Binary = "claude"
	}
	return &Harness{binary: opts.Binary}
}

// Name implements harness.Harness.
func (h *Harness) Name() string { return Name }

// DefaultProfiles are the roles' settings until the pipeline config sets
// them (spec sections 7.1 and 7.3).
func DefaultProfiles() map[string]harness.Profile {
	readOnly := []string{"Read", "Grep", "Glob", "Bash(istari *)", "Bash(git log *)", "Bash(git diff *)", "Bash(git show *)"}
	return map[string]harness.Profile{
		"refiner":  {Harness: Name, Model: "claude-opus-5-5", PermissionMode: "default", AllowedTools: readOnly},
		"planner":  {Harness: Name, Model: "claude-opus-5-5", PermissionMode: "default", AllowedTools: readOnly},
		"reviewer": {Harness: Name, Model: "claude-opus-5-5", PermissionMode: "default", AllowedTools: readOnly},
		"implementer": {Harness: Name, Model: "claude-opus-5-5", PermissionMode: "acceptEdits",
			AllowedTools: []string{"Read", "Edit", "Write", "Grep", "Glob", "Bash(istari *)", "Bash(git *)"}},
	}
}

// Check implements harness.Harness: the binary must exist and run.
func (h *Harness) Check(ctx context.Context) error {
	path, err := exec.LookPath(h.binary)
	if err != nil {
		return fmt.Errorf("%s: the %q binary was not found in PATH; install Claude Code (https://docs.claude.com/claude-code) or put it on the daemon's PATH", Name, h.binary)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s --version failed: %v: %s", Name, path, err, out)
	}
	return nil
}

// Start implements harness.Harness.
func (h *Harness) Start(ctx context.Context, spec harness.RunSpec) (harness.Session, error) {
	return h.start(ctx, spec, "")
}

// Resume implements harness.Resumer: the brief is sent into the old session.
func (h *Harness) Resume(ctx context.Context, sessionID string, spec harness.RunSpec) (harness.Session, error) {
	if sessionID == "" {
		return nil, errors.New("claudecode: resume needs a session id")
	}
	return h.start(ctx, spec, sessionID)
}

// args builds the command line of spec section 7.3. stream-json output with
// --print requires --verbose.
func args(spec harness.RunSpec, resume string) []string {
	a := []string{"--print", spec.Brief, "--output-format", "stream-json", "--verbose"}
	if resume != "" {
		a = append(a, "--resume", resume)
	}
	if spec.Instructions != "" {
		a = append(a, "--append-system-prompt", spec.Instructions)
	}
	if spec.Model != "" {
		a = append(a, "--model", spec.Model)
	}
	if spec.PermissionMode != "" {
		a = append(a, "--permission-mode", spec.PermissionMode)
	}
	if len(spec.AllowedTools) > 0 {
		a = append(a, "--allowedTools")
		a = append(a, spec.AllowedTools...)
	}
	if spec.MaxBudgetUSD > 0 {
		a = append(a, "--max-budget-usd", strconv.FormatFloat(spec.MaxBudgetUSD, 'f', -1, 64))
	}
	return a
}

// baseEnv are the variables of the daemon's environment a harness needs to
// run at all. Everything else, such as tracker or GitHub tokens, stays with
// the daemon (spec section 11).
var baseEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TMPDIR", "TERM"}

func environment(spec harness.RunSpec) []string {
	var env []string
	for _, k := range baseEnv {
		if v, ok := os.LookupEnv(k); ok {
			if _, own := spec.Env[k]; !own {
				env = append(env, k+"="+v)
			}
		}
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+spec.Env[k])
	}
	return env
}

func (h *Harness) start(ctx context.Context, spec harness.RunSpec, resume string) (harness.Session, error) {
	cmd := exec.Command(h.binary, args(spec, resume)...)
	cmd.Dir = spec.Workdir
	cmd.Env = environment(spec)
	cmd.Stdin = nil
	// A process group of its own, so that Stop reaches the shells and tools
	// the agent started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudecode: start %s: %w", h.binary, err)
	}
	s := &session{cmd: cmd, events: make(chan harness.Event, 64), done: make(chan struct{}), seen: map[string]bool{}}
	go s.read(stdout)
	go func() {
		select {
		case <-ctx.Done():
			s.Stop(10 * time.Second)
		case <-s.done:
		}
	}()
	return s, nil
}

type session struct {
	cmd    *exec.Cmd
	events chan harness.Event
	done   chan struct{} // closed when the process has exited

	mu     sync.Mutex
	id     string
	seen   map[string]bool // message ids whose usage is counted
	summed harness.Usage   // per-message sum
	final  *harness.Usage  // the result line's totals, if any
	result harness.Result
	err    error
}

func (s *session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *session) Events() <-chan harness.Event { return s.events }

func (s *session) Send(string) error { return harness.ErrSendNotSupported }

// Stop sends SIGTERM to the process group and SIGKILL after grace.
func (s *session) Stop(grace time.Duration) error {
	pgid := -s.cmd.Process.Pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	select {
	case <-s.done:
		return nil
	case <-time.After(grace):
	}
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	<-s.done
	return nil
}

func (s *session) Wait() (harness.Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

// streamLine is one line of stream-json output.
type streamLine struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Message   struct {
		ID      string `json:"id"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage *apiUsage `json:"usage"`
	} `json:"message"`
	Usage        *apiUsage `json:"usage"`
	TotalCostUSD float64   `json:"total_cost_usd"`
}

type apiUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u apiUsage) usage() harness.Usage {
	return harness.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens}
}

// read turns the output into events, then waits for the process. A message
// whose content spans several lines repeats its usage on each; it is counted
// once, by message id. The result line's totals win over the sum.
func (s *session) read(stdout interface{ Read([]byte) (int, error) }) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var line streamLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue // not a stream line, e.g. a warning
		}
		s.mu.Lock()
		if line.SessionID != "" {
			s.id = line.SessionID
		}
		s.mu.Unlock()
		switch line.Type {
		case "assistant":
			for _, c := range line.Message.Content {
				switch c.Type {
				case "text":
					s.events <- harness.Event{Kind: harness.EventMessage, Text: c.Text}
				case "tool_use":
					s.events <- harness.Event{Kind: harness.EventToolCall, Tool: c.Name, ToolInput: c.Input}
				}
			}
			if u := line.Message.Usage; u != nil && line.Message.ID != "" {
				s.mu.Lock()
				first := !s.seen[line.Message.ID]
				s.seen[line.Message.ID] = true
				if first {
					s.summed = s.summed.Add(u.usage())
				}
				s.mu.Unlock()
				if first {
					s.events <- harness.Event{Kind: harness.EventUsage, Usage: u.usage()}
				}
			}
		case "result":
			if line.Usage != nil {
				total := line.Usage.usage()
				total.CostUSD = line.TotalCostUSD
				s.mu.Lock()
				s.final = &total
				s.mu.Unlock()
			}
		}
	}
	readErr := sc.Err()
	close(s.events)
	waitErr := s.cmd.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.result.SessionID = s.id
	s.result.Usage = s.summed
	if s.final != nil {
		s.result.Usage = *s.final
	}
	var exit *exec.ExitError
	switch {
	case waitErr == nil:
		s.result.ExitCode = 0
	case errors.As(waitErr, &exit):
		s.result.ExitCode = exit.ExitCode() // -1 when killed by a signal
	default:
		s.err = waitErr
	}
	if s.err == nil && readErr != nil {
		s.err = fmt.Errorf("claudecode: reading the stream: %w", readErr)
	}
	close(s.done)
}
