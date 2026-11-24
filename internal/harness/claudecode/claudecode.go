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
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
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

// readGrace is how long the output may stay open after the harness exited:
// a background process the agent started can hold it; then it is closed.
const readGrace = 2 * time.Second

// Harness starts Claude Code runs.
type Harness struct {
	binary string
}

var (
	_ harness.Harness = (*Harness)(nil)
	_ harness.Resumer = (*Harness)(nil)
)

func init() {
	harness.Register(New(Options{}))
	harness.RegisterDefaults(DefaultProfiles())
}

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
		// dontAsk: a tool that is not allowed is refused, nobody is asked.
		"refiner":  {Harness: Name, Model: "claude-opus-5-5", PermissionMode: "dontAsk", AllowedTools: readOnly},
		"planner":  {Harness: Name, Model: "claude-opus-5-5", PermissionMode: "dontAsk", AllowedTools: readOnly},
		"reviewer": {Harness: Name, Model: "claude-opus-5-5", PermissionMode: "dontAsk", AllowedTools: readOnly},
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
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = environment(harness.RunSpec{}) // the environment a run gets
	if out, err := cmd.CombinedOutput(); err != nil {
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
// --print requires --verbose. The brief comes last, after --, so that it is
// never read as a flag.
func args(spec harness.RunSpec, resume string) []string {
	a := []string{"--print", "--output-format", "stream-json", "--verbose"}
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
	if spec.Guard != "" {
		// Settings given on the command line rank above the repository's,
		// so a file in the worktree cannot switch the hook off.
		settings, _ := json.Marshal(map[string]any{
			"disableAllHooks": false,
			"hooks": map[string]any{"PreToolUse": []any{map[string]any{
				"matcher": hookMatcher,
				"hooks":   []any{map[string]string{"type": "command", "command": spec.Guard + " --harness " + Name}},
			}}},
		})
		a = append(a, "--settings", string(settings))
	}
	return append(a, "--", spec.Brief)
}

// hookMatcher are the tools the guard sees: the shell, and the file tools,
// which could write into a git directory.
const hookMatcher = "Bash|Write|Edit|MultiEdit|NotebookEdit"

// ReadHook implements harness.HookReader: Claude Code's PreToolUse input.
func (h *Harness) ReadHook(raw []byte) (harness.HookCall, error) {
	var in struct {
		Tool  string `json:"tool_name"`
		Cwd   string `json:"cwd"`
		Input struct {
			Command      string `json:"command"`
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return harness.HookCall{}, fmt.Errorf("claudecode: the hook input: %w", err)
	}
	call := harness.HookCall{Cwd: in.Cwd}
	switch in.Tool {
	case "Bash":
		call.Command = in.Input.Command
	case "Write", "Edit", "MultiEdit":
		call.Path = in.Input.FilePath
	case "NotebookEdit":
		call.Path = in.Input.NotebookPath
	}
	return call, nil
}

// baseEnv are the variables of the daemon's environment a harness needs to
// run at all; passPrefixes are those of the model provider and of the
// harness itself. Everything else, such as the Slack, tracker or GitHub
// tokens, stays with the daemon (spec section 11).
var (
	baseEnv      = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TMPDIR", "TERM", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"}
	passPrefixes = []string{"ANTHROPIC_", "CLAUDE_CODE_"}
)

func passed(key string) bool {
	if slices.Contains(baseEnv, key) {
		return true
	}
	for _, p := range passPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func environment(spec harness.RunSpec) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, own := spec.Env[k]; passed(k) && !own {
			env = append(env, kv)
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
	// The output goes to a pipe of our own, not one exec manages, so that
	// waiting for the process never depends on reading its output.
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(h.binary, args(spec, resume)...)
	cmd.Dir = spec.Workdir
	cmd.Env = environment(spec)
	cmd.Stdout = w
	// A process group of its own, so that Stop reaches the shells and tools
	// the agent started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("claudecode: start %s: %w", h.binary, err)
	}
	w.Close()
	s := &session{
		cmd: cmd, out: r, queue: newQueue(),
		exited: make(chan struct{}), read: make(chan struct{}), done: make(chan struct{}),
		seen: map[string]bool{},
	}
	go s.readOutput()
	go s.wait()
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
	out    *os.File
	queue  *queue
	exited chan struct{} // the process has exited
	read   chan struct{} // the output has been read to its end
	done   chan struct{} // both; the result is final

	mu       sync.Mutex
	id       string
	seen     map[string]bool // message ids whose usage is counted
	summed   harness.Usage   // per-message sum
	final    *harness.Usage  // the result line's totals, if any
	failed   bool
	reason   string
	waitErr  error
	readErr  error
	result   harness.Result
	finalErr error
}

func (s *session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *session) Events() <-chan harness.Event { return s.queue.out }

func (s *session) Send(string) error { return harness.ErrSendNotSupported }

// Stop sends SIGTERM to the process group and SIGKILL after grace. It does
// nothing once the process has exited: its group id may belong to another
// process by then.
func (s *session) Stop(grace time.Duration) error {
	select {
	case <-s.exited:
		return nil
	default:
	}
	pgid := -s.cmd.Process.Pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	select {
	case <-s.exited:
		return nil
	case <-time.After(grace):
	}
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	<-s.exited
	return nil
}

func (s *session) Wait() (harness.Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.finalErr
}

// wait reaps the process, gives the output readGrace to end, closes it if a
// background process still holds it, and then fixes the result.
func (s *session) wait() {
	err := s.cmd.Wait()
	s.mu.Lock()
	s.waitErr = err
	s.mu.Unlock()
	close(s.exited)
	select {
	case <-s.read:
	case <-time.After(readGrace):
		s.out.Close() // ends readOutput
		<-s.read
	}
	s.finish()
}

func (s *session) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result.SessionID = s.id
	s.result.Usage = s.summed
	if s.final != nil {
		s.result.Usage = *s.final
	}
	s.result.Failed, s.result.Reason = s.failed, s.reason
	var exit *exec.ExitError
	switch {
	case s.waitErr == nil:
		s.result.ExitCode = 0
	case errors.As(s.waitErr, &exit):
		s.result.ExitCode = exit.ExitCode() // -1 when killed by a signal
	default:
		s.finalErr = s.waitErr
	}
	if s.finalErr == nil && s.readErr != nil && !errors.Is(s.readErr, os.ErrClosed) {
		s.finalErr = fmt.Errorf("claudecode: reading the stream: %w", s.readErr)
	}
	close(s.done)
}

// streamLine is one line of stream-json output.
type streamLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
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

// readOutput turns the output into events. A message whose content spans
// several lines repeats its usage on each; it is counted once, by message id
// (a message without an id counts every time). The result line's totals win
// over the sum. Events never wait for a reader: they queue.
func (s *session) readOutput() {
	defer close(s.read)
	defer s.queue.close()
	sc := bufio.NewScanner(s.out)
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
					s.queue.push(harness.Event{Kind: harness.EventMessage, Text: c.Text})
				case "tool_use":
					s.queue.push(harness.Event{Kind: harness.EventToolCall, Tool: c.Name, ToolInput: c.Input})
				}
			}
			if u := line.Message.Usage; u != nil {
				s.mu.Lock()
				first := line.Message.ID == "" || !s.seen[line.Message.ID]
				s.seen[line.Message.ID] = true
				if first {
					s.summed = s.summed.Add(u.usage())
				}
				s.mu.Unlock()
				if first {
					s.queue.push(harness.Event{Kind: harness.EventUsage, Usage: u.usage()})
				}
			}
		case "result":
			s.mu.Lock()
			s.failed, s.reason = line.IsError, line.Subtype
			if line.Usage != nil {
				total := line.Usage.usage()
				total.CostUSD = line.TotalCostUSD
				s.final = &total
			}
			s.mu.Unlock()
		}
	}
	if err := sc.Err(); err != nil {
		s.mu.Lock()
		s.readErr = err
		s.mu.Unlock()
		io.Copy(io.Discard, s.out) // keep the harness from blocking on a full pipe
	}
}

// queue hands events to a reader in order without ever blocking the writer.
type queue struct {
	in  chan harness.Event
	out chan harness.Event
}

func newQueue() *queue {
	q := &queue{in: make(chan harness.Event), out: make(chan harness.Event)}
	go q.run()
	return q
}

func (q *queue) push(e harness.Event) { q.in <- e }
func (q *queue) close()               { close(q.in) }

func (q *queue) run() {
	defer close(q.out)
	var pending []harness.Event
	in := q.in
	for in != nil || len(pending) > 0 {
		var out chan harness.Event
		var next harness.Event
		if len(pending) > 0 {
			out, next = q.out, pending[0]
		}
		select {
		case e, ok := <-in:
			if !ok {
				in = nil
				continue
			}
			pending = append(pending, e)
		case out <- next:
			pending = pending[1:]
		}
	}
}
