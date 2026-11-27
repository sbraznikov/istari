package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/exectest"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/harness/claudecode"
)

var ctx = context.Background()

// fakeClaude is a stand-in for the claude binary. It records its arguments,
// working directory and environment, then acts by FAKE_MODE: replay a stream
// and exit with FAKE_EXIT, or hang until it is stopped.
const fakeClaude = `#!/bin/sh
out="$FAKE_DIR"
printf '%s\n' "$@" > "$out/args"
pwd > "$out/pwd"
env > "$out/env"
case "$FAKE_MODE" in
stream)
	cat "$FAKE_STREAM"
	exit "${FAKE_EXIT:-0}"
	;;
exit-on-term)
	trap 'echo TERM >> "$out/signals"; exit 143' TERM
	echo ready > "$out/ready"
	while :; do sleep 0.05; done
	;;
ignore-term)
	trap 'echo TERM >> "$out/signals"' TERM
	echo ready > "$out/ready"
	while :; do sleep 0.05; done
	;;
flood)
	i=0
	while [ $i -lt 500 ]; do
		echo '{"type":"assistant","message":{"id":"m'$i'","content":[{"type":"text","text":"x"}]}}'
		i=$((i+1))
	done
	;;
orphan)
	cat "$FAKE_STREAM"
	sleep 30 &
	exit 0
	;;
esac
`

type fake struct {
	bin, dir string
}

func newFake(t *testing.T) fake {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	exectest.Write(t, bin, fakeClaude)
	return fake{bin: bin, dir: dir}
}

func (f fake) read(t *testing.T, name string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(f.dir, name))
	return string(data)
}

func (f fake) spec(t *testing.T, mode string, extra map[string]string) harness.RunSpec {
	t.Helper()
	stream, _ := filepath.Abs("testdata/stream.jsonl")
	env := map[string]string{"FAKE_DIR": f.dir, "FAKE_MODE": mode, "FAKE_STREAM": stream, "ISTARI_TICKET": "gh:acme/web#1"}
	for k, v := range extra {
		env[k] = v
	}
	return harness.RunSpec{
		Workdir:        t.TempDir(),
		Brief:          "Implement gh:acme/web#1.",
		Instructions:   "You are the implementer.",
		Env:            env,
		Model:          "claude-opus-5-5",
		PermissionMode: "acceptEdits",
		AllowedTools:   []string{"Read", "Edit", "Bash(istari *)"},
		MaxBudgetUSD:   5,
	}
}

func collect(t *testing.T, s harness.Session) []harness.Event {
	t.Helper()
	var events []harness.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				return events
			}
			events = append(events, e)
		case <-timeout:
			t.Fatal("the event stream did not end")
		}
	}
}

func TestTheAdapterStartsClaudeHeadlessAndYieldsEveryKindOfEventInOrder(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	h := claudecode.New(claudecode.Options{Binary: f.bin})
	spec := f.spec(t, "stream", nil)
	s, err := h.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := collect(t, s)
	var kinds []harness.EventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []harness.EventKind{harness.EventMessage, harness.EventUsage, harness.EventToolCall, harness.EventMessage, harness.EventUsage}
	if !slices.Equal(kinds, want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	if events[0].Text != "I will read the ticket first." || events[3].Text != "The ticket is ready; done." {
		t.Errorf("messages = %q, %q", events[0].Text, events[3].Text)
	}
	var input struct{ Command string }
	if err := json.Unmarshal(events[2].ToolInput, &input); err != nil {
		t.Fatal(err)
	}
	if events[2].Tool != "Bash" || input.Command != "istari ticket show gh:acme/web#1" {
		t.Errorf("tool call = %s %s", events[2].Tool, events[2].ToolInput)
	}
	if s.ID() != "sess-42" {
		t.Errorf("session id = %q, want sess-42", s.ID())
	}

	args := strings.Split(strings.TrimSpace(f.read(t, "args")), "\n")
	for _, w := range [][]string{
		{"--", "Implement gh:acme/web#1."}, // the brief, after --: it may start with a dash
		{"--append-system-prompt", "You are the implementer."},
		{"--output-format", "stream-json"},
		{"--model", "claude-opus-5-5"},
		{"--permission-mode", "acceptEdits"},
		{"--allowedTools", "Read", "Edit", "Bash(istari *)"},
		{"--max-budget-usd", "5"},
	} {
		if !containsRun(args, w) {
			t.Errorf("arguments %q lack %q", args, w)
		}
	}
	if !slices.Contains(args, "--print") || args[len(args)-1] != "Implement gh:acme/web#1." {
		t.Errorf("arguments %q: want --print and the brief last", args)
	}
	if !slices.Contains(args, "--verbose") {
		t.Errorf("arguments %q lack --verbose, which stream-json with --print requires", args)
	}
	if got := strings.TrimSpace(f.read(t, "pwd")); !samePath(got, spec.Workdir) {
		t.Errorf("started in %s, want the workdir %s", got, spec.Workdir)
	}
}

func containsRun(args, run []string) bool {
	for i := range args {
		if i+len(run) <= len(args) && slices.Equal(args[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra == rb
}

func TestUsageIsSummedOncePerMessageAndTheExitCodeIsSurfaced(t *testing.T) {
	t.Parallel()
	want := harness.Usage{InputTokens: 2700, OutputTokens: 65, CacheReadTokens: 800, CacheWriteTokens: 100}
	f := newFake(t)
	h := claudecode.New(claudecode.Options{Binary: f.bin})

	s, err := h.Start(ctx, f.spec(t, "stream", map[string]string{"FAKE_EXIT": "3"}))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	res, err := s.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	withCost := want
	withCost.CostUSD = 0.0421
	if res.Usage != withCost {
		t.Errorf("usage = %+v, want %+v", res.Usage, withCost)
	}

	// Without the final result line (the process died), the totals are the
	// sum over messages, each message counted once although it spans lines.
	truncated := filepath.Join(t.TempDir(), "stream.jsonl")
	full, _ := os.ReadFile("testdata/stream.jsonl")
	lines := strings.Split(strings.TrimSpace(string(full)), "\n")
	if err := os.WriteFile(truncated, []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = h.Start(ctx, f.spec(t, "stream", map[string]string{"FAKE_STREAM": truncated, "FAKE_EXIT": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	if res, _ = s.Wait(); res.Usage != want || res.ExitCode != 1 {
		t.Errorf("without a result line: usage %+v exit %d, want %+v exit 1", res.Usage, res.ExitCode, want)
	}
}

func TestStopSendsSIGTERMThenSIGKILLAfterTheGracePeriod(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	h := claudecode.New(claudecode.Options{Binary: f.bin})

	s, err := h.Start(ctx, f.spec(t, "exit-on-term", nil))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.read(t, "ready") != "" }) // its trap is set
	start := time.Now()
	if err := s.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("a process that exits on SIGTERM took %s to stop", took)
	}
	if !strings.Contains(f.read(t, "signals"), "TERM") {
		t.Error("the process did not receive SIGTERM")
	}

	f2 := newFake(t)
	h2 := claudecode.New(claudecode.Options{Binary: f2.bin})
	s, err = h2.Start(ctx, f2.spec(t, "ignore-term", nil))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f2.read(t, "ready") != "" })
	start = time.Now()
	if err := s.Stop(300 * time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	took := time.Since(start)
	if took < 300*time.Millisecond || took > 5*time.Second {
		t.Errorf("a process that ignores SIGTERM stopped after %s, want just after the 300ms grace", took)
	}
	if !strings.Contains(f2.read(t, "signals"), "TERM") {
		t.Error("the process did not receive SIGTERM before SIGKILL")
	}
	res, _ := s.Wait()
	if res.ExitCode != -1 {
		t.Errorf("exit code after SIGKILL = %d, want -1 (killed by a signal)", res.ExitCode)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTheAgentGetsOnlyItsOwnEnvironment(t *testing.T) {
	// Serial: sets GITHUB_TOKEN with t.Setenv.
	t.Setenv("GITHUB_TOKEN", "daemon-secret")
	f := newFake(t)
	s, err := claudecode.New(claudecode.Options{Binary: f.bin}).Start(ctx, f.spec(t, "stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	env := f.read(t, "env")
	if strings.Contains(env, "daemon-secret") {
		t.Fatal("the daemon's GITHUB_TOKEN reached the agent")
	}
	if !strings.Contains(env, "ISTARI_TICKET=gh:acme/web#1") || !strings.Contains(env, "PATH=") {
		t.Fatalf("the agent's environment lacks its own variables or PATH:\n%s", env)
	}
}

func TestTheAgentFindsTheDaemonsOwnBinaryFirstOnItsPath(t *testing.T) {
	// Serial: sets PATH with t.Setenv. The test binary stands in for the
	// istari binary the daemon runs from; its directory is not on PATH.
	t.Setenv("PATH", "/usr/bin:/bin")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	s, err := claudecode.New(claudecode.Options{Binary: f.bin}).Start(ctx, f.spec(t, "stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	want := "PATH=" + filepath.Dir(self) + ":/usr/bin:/bin\n"
	if env := f.read(t, "env"); !strings.Contains("\n"+env, "\n"+want) {
		t.Fatalf("the agent's environment lacks %q:\n%s", want, env)
	}
}

func TestWithoutADaemonPathTheAgentStillFindsTheDaemonsOwnBinary(t *testing.T) {
	// Serial: unsets PATH. The agent's PATH then holds only the test
	// binary's directory, so the fake names env by absolute path and runs
	// in a mode that does nothing else.
	t.Setenv("PATH", "")
	if err := os.Unsetenv("PATH"); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	script := strings.Replace(fakeClaude, "env > ", "/usr/bin/env > ", 1)
	exectest.Write(t, f.bin, script)
	s, err := claudecode.New(claudecode.Options{Binary: f.bin}).Start(ctx, f.spec(t, "exit", nil))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	want := "PATH=" + filepath.Dir(self) + "\n"
	if env := f.read(t, "env"); !strings.Contains("\n"+env, "\n"+want) {
		t.Fatalf("the agent's environment lacks %q:\n%s", want, env)
	}
}

func TestSendIsNotSupportedInPrintMode(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	s, err := claudecode.New(claudecode.Options{Binary: f.bin}).Start(ctx, f.spec(t, "stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("hello"); !errors.Is(err, harness.ErrSendNotSupported) {
		t.Fatalf("Send = %v, want ErrSendNotSupported", err)
	}
	collect(t, s)
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestResumeContinuesTheSession(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	h := claudecode.New(claudecode.Options{Binary: f.bin})
	var r harness.Resumer = h
	s, err := r.Resume(ctx, "sess-42", f.spec(t, "stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if args := strings.Split(f.read(t, "args"), "\n"); !containsRun(args, []string{"--resume", "sess-42"}) {
		t.Fatalf("arguments %q lack --resume sess-42", args)
	}
}

func TestCheckReportsAMissingBinaryClearly(t *testing.T) {
	t.Parallel()
	h := claudecode.New(claudecode.Options{Binary: filepath.Join(t.TempDir(), "no-such-claude")})
	err := h.Check(ctx)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Check = %v, want a not-found message", err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	f := newFake(t)
	if err := claudecode.New(claudecode.Options{Binary: f.bin}).Check(ctx); err != nil {
		t.Fatalf("Check with a binary that runs: %v", err)
	}
}

func TestACallerThatDoesNotReadEventsCanStillWaitAndStop(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	h := claudecode.New(claudecode.Options{Binary: f.bin})
	s, err := h.Start(ctx, f.spec(t, "flood", nil)) // more events than any buffer
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan harness.Result, 1)
	go func() { res, _ := s.Wait(); done <- res }()
	select {
	case res := <-done:
		if res.ExitCode != 0 {
			t.Fatalf("exit code = %d", res.ExitCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait hung while nobody read the events")
	}
	if err := s.Stop(time.Second); err != nil { // after the exit: a no-op
		t.Fatalf("Stop after exit: %v", err)
	}
}

func TestABackgroundChildHoldingTheOutputDoesNotHoldTheSession(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	s, err := claudecode.New(claudecode.Options{Binary: f.bin}).Start(ctx, f.spec(t, "orphan", nil))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []harness.Event, 1)
	go func() { done <- collect(t, s) }()
	select {
	case events := <-done:
		if len(events) != 5 {
			t.Fatalf("got %d events before the stream was closed, want all 5", len(events))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the event stream stayed open after the harness exited")
	}
	if res, err := s.Wait(); err != nil || res.ExitCode != 0 {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
}

func TestCancellingTheContextStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	runCtx, cancel := context.WithCancel(ctx)
	s, err := claudecode.New(claudecode.Options{Binary: f.bin}).Start(runCtx, f.spec(t, "exit-on-term", nil))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.read(t, "ready") != "" })
	cancel()
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }() //nolint:errcheck // a canceled run: only that it ends counts
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the context did not stop the run")
	}
	if !strings.Contains(f.read(t, "signals"), "TERM") {
		t.Fatal("the run was not sent SIGTERM")
	}
}

func TestTheModelProvidersCredentialsReachTheHarness(t *testing.T) {
	// Serial: sets ANTHROPIC_API_KEY, HTTPS_PROXY and SLACK_BOT_TOKEN with t.Setenv.
	t.Setenv("ANTHROPIC_API_KEY", "model-key")
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("SLACK_BOT_TOKEN", "daemon-secret")
	f := newFake(t)
	s, err := claudecode.New(claudecode.Options{Binary: f.bin}).Start(ctx, f.spec(t, "stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	env := f.read(t, "env")
	for _, want := range []string{"ANTHROPIC_API_KEY=model-key", "HTTPS_PROXY=http://proxy:3128"} {
		if !strings.Contains(env, want) {
			t.Errorf("the harness lacks %s", want)
		}
	}
	if strings.Contains(env, "daemon-secret") {
		t.Error("the daemon's Slack token reached the agent")
	}
}

func TestAResultLineThatReportsAnErrorIsSurfaced(t *testing.T) {
	t.Parallel()
	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(stream, []byte(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"session_id":"s","total_cost_usd":5.01,"usage":{"input_tokens":1,"output_tokens":1}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	s, err := claudecode.New(claudecode.Options{Binary: f.bin}).Start(ctx, f.spec(t, "stream", map[string]string{"FAKE_STREAM": stream, "FAKE_EXIT": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	res, _ := s.Wait()
	if !res.Failed || res.Reason != "error_max_budget_usd" || res.Usage.CostUSD != 5.01 {
		t.Fatalf("result = %+v, want a failed run with its reason and cost", res)
	}
}

func TestTheRolesDefaultsUseModesTheHarnessAccepts(t *testing.T) {
	t.Parallel()
	// The modes of claude --permission-mode (Claude Code 2.1).
	valid := []string{"acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"}
	profiles := claudecode.DefaultProfiles()
	for _, role := range []string{"refiner", "planner", "implementer", "reviewer"} {
		p, ok := profiles[role]
		if !ok {
			t.Fatalf("no profile for %s", role)
		}
		if !slices.Contains(valid, p.PermissionMode) || p.Model == "" || p.Harness != claudecode.Name {
			t.Errorf("%s profile = %+v", role, p)
		}
		spec := p.Apply(harness.RunSpec{Brief: "b"})
		if spec.PermissionMode != p.PermissionMode || spec.Model != p.Model || !slices.Equal(spec.AllowedTools, p.AllowedTools) || spec.Brief != "b" {
			t.Errorf("Apply(%s) = %+v", role, spec)
		}
	}
	if slices.Contains(profiles["reviewer"].AllowedTools, "Edit") {
		t.Error("the reviewer may edit files")
	}
}

func TestOnlyTheReviewerAmongTheReadOnlyRolesMayFetchTheMainBranch(t *testing.T) {
	t.Parallel()
	profiles := claudecode.DefaultProfiles()
	if !slices.Contains(profiles["reviewer"].AllowedTools, "Bash(git fetch origin main)") {
		t.Errorf("reviewer tools = %q, want git fetch origin main", profiles["reviewer"].AllowedTools)
	}
	for _, role := range []string{"refiner", "planner"} {
		for _, tool := range profiles[role].AllowedTools {
			if strings.Contains(tool, "git fetch") {
				t.Errorf("the %s may run %s", role, tool)
			}
		}
	}
}

func TestTheGuardRunsAsAHookBeforeEveryShellCommandAndCannotBeSwitchedOff(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	h := claudecode.New(claudecode.Options{Binary: f.bin})
	spec := f.spec(t, "stream", nil)
	spec.Guard = "'/opt/istari bin/istari' hook git-guard"
	s, err := h.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	args := strings.Split(strings.TrimSpace(f.read(t, "args")), "\n")
	i := slices.Index(args, "--settings")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("arguments %q lack --settings", args)
	}
	var settings struct {
		DisableAllHooks *bool `json:"disableAllHooks"`
		Hooks           struct {
			PreToolUse []struct {
				Matcher string
				Hooks   []struct{ Type, Command string }
			}
		}
	}
	if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
		t.Fatalf("settings %q: %v", args[i+1], err)
	}
	pre := settings.Hooks.PreToolUse
	if len(pre) != 1 || pre[0].Matcher != "Bash|Write|Edit|MultiEdit|NotebookEdit" || len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Type != "command" || pre[0].Hooks[0].Command != spec.Guard+" --harness claude-code" {
		t.Fatalf("hooks = %+v", settings.Hooks)
	}
	if settings.DisableAllHooks == nil || *settings.DisableAllHooks {
		t.Fatal("the settings leave a repository free to switch the hooks off")
	}
}
