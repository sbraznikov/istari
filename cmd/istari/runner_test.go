package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// fakeAgent is a stand-in for the harness binary: it reports what it was
// given, acts as an agent through istari, then prints a short stream.
const fakeAgent = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
out=%[1]q
istari=%[2]q
env | grep '^ISTARI_' | sort > "$out/env"
env > "$out/allenv"
printf '%%s\n' "$@" > "$out/args"
git -c user.name=agent -c user.email=agent@example.com commit -q --allow-empty -m "work" > /dev/null 2>&1
push() { curl -s -o /dev/null -w "%%{http_code}" --unix-socket "$ISTARI_SOCKET" -H "Authorization: Bearer $ISTARI_TOKEN" -d "$1" http://istari/v1/vcs.push; }
push '{"actor":"x","branch":"main"}' > "$out/push_main"
push '{"actor":"x","branch":"istari/6/other"}' > "$out/push_other"
push '{"actor":"x"}' > "$out/push_own"
echo done > feature.txt
"$istari" checkpoint "Did the work; next nothing" > "$out/checkpoint" 2>&1
pwd > "$out/pwd"
"$istari" whoami --json > "$out/whoami" 2>&1
"$istari" ticket show gh:acme/web#99 > /dev/null 2> "$out/scope"; echo "exit $?" >> "$out/scope"
"$istari" ticket priority gh:acme/web#5 P0 > /dev/null 2> "$out/priority"; echo "exit $?" >> "$out/priority"
echo '{"type":"system","subtype":"init","session_id":"s1","model":"m","cwd":"/w","tools":[]}'
echo '{"type":"assistant","session_id":"s1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"working on it"}],"usage":{"input_tokens":1,"output_tokens":1}}}'
sleep 2
"$istari" complete "all done" > "$out/complete" 2>&1
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestTheRunnerRunsAnAgentInTheTicketWorktreeWithItsToken(t *testing.T) {
	server := gitServer(t)
	dataDir := newDataDir(t)
	s, err := store.Open(filepath.Join(dataDir, api.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := core.NewTickets(s, core.TicketOptions{})
	if _, err := k.CreateWith(ctx, "human:alex", core.NewTicket{Ref: "gh:acme/web#5", Project: "web", Title: "Add limits"}); err != nil {
		t.Fatal(err)
	}
	// in_progress: the pipeline has nothing to start; only this task runs.
	for _, tr := range []string{"accept", "start", "plan_accepted"} {
		if _, err := k.Fire(ctx, "system", "gh:acme/web#5", tr); err != nil {
			t.Fatal(err)
		}
	}
	task, err := core.NewTasks(s, core.TaskOptions{}).Create(ctx, "system", core.NewTask{Ticket: "gh:acme/web#5", Project: "web", Role: "implementer"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	out := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(fakeAgent, out, binary)), 0o755); err != nil {
		t.Fatal(err)
	}
	gh := githubtest.New(t)
	for range 5 { // issue #5 is the ticket: its journal goes there; closed, they are not synced or refined
		gh.HumanClose(gh.Seed("Add limits", "## Context\n\nLimits.\n", "bob"), "bob", "completed")
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server,
		"PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)

	// The run appears; its transcript can be followed until it ends.
	var runs []struct{ ID, State string }
	for deadline := time.Now().Add(20 * time.Second); len(runs) == 0; time.Sleep(50 * time.Millisecond) {
		d.mustCLI(t, &runs, "run", "list")
		if time.Now().After(deadline) {
			t.Fatalf("no run started\nstderr: %s", d.stderr)
		}
	}
	follow := exec.Command(binary, "--config", d.config, "run", "logs", "-f", runs[0].ID)
	logs, err := follow.Output()
	if err != nil || !strings.Contains(string(logs), "working on it") {
		t.Fatalf("run logs -f = %q, %v", logs, err)
	}

	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var got struct{ State string }
		d.mustCLI(t, &got, "task", "show", task.ID)
		if got.State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task is %s, want completed\nstderr: %s", got.State, d.stderr)
		}
	}

	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(out, name)); return string(b) }
	env := read("env")
	for _, want := range []string{"ISTARI_PROFILE=implementer-1", "ISTARI_ROLE=implementer", "ISTARI_TICKET=gh:acme/web#5", "ISTARI_TASK=" + task.ID, "ISTARI_TOKEN="} {
		if !strings.Contains(env, want) {
			t.Errorf("the agent's environment lacks %s:\n%s", want, env)
		}
	}
	// The daemon hands the harness its git guard; run it as the harness
	// would, on a push from the worktree.
	args := strings.Split(read("args"), "\n")
	i := slices.Index(args, "--settings")
	var settings struct {
		Hooks struct {
			PreToolUse []struct{ Hooks []struct{ Command string } }
		}
	}
	if i < 0 || json.Unmarshal([]byte(args[i+1]), &settings) != nil || len(settings.Hooks.PreToolUse) == 0 {
		t.Fatalf("the harness got no git guard: %q", args)
	}
	if j := slices.Index(args, "--max-budget-usd"); j < 0 || args[j+1] != "15" {
		t.Errorf("the run of a ticket without a size is not capped at the M budget of $15: %q", args)
	}
	wt := strings.TrimSpace(read("pwd"))
	guard := exec.Command("sh", "-c", settings.Hooks.PreToolUse[0].Hooks[0].Command)
	guard.Env = append(os.Environ(), "ISTARI_WORKTREE="+wt)
	guard.Stdin = strings.NewReader(`{"tool_name":"Bash","cwd":"` + wt + `","tool_input":{"command":"git push"}}`)
	if out, err := guard.CombinedOutput(); guard.ProcessState == nil || guard.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "istari checkpoint") {
		t.Errorf("the guard on git push: %v %s", err, out)
	}
	if pwd := strings.TrimSpace(read("pwd")); !strings.HasSuffix(pwd, filepath.Join("workspaces", "web", "web", "5")) {
		t.Errorf("the agent ran in %s, want the ticket worktree", pwd)
	}
	if w := read("whoami"); !strings.Contains(w, `"role":"implementer"`) || !strings.Contains(w, "push to istari/5/add-limits") || !strings.Contains(w, "lease_remaining") {
		t.Errorf("whoami = %s", w)
	}
	if scope := read("scope"); !strings.Contains(scope, "token_scope") || !strings.Contains(scope, "exit 1") {
		t.Errorf("acting on another ticket: %s", scope)
	}
	if got := read("push_main") + " " + read("push_other") + " " + read("push_own"); got != "403 403 200" {
		t.Errorf("pushes to main, another ticket's branch and its own: %s, want 403 403 200", got)
	}
	if out, err := exec.Command("git", "-C", filepath.Join(server, "acme", "web.git"), "rev-parse", "--verify", "istari/5/add-limits").CombinedOutput(); err != nil {
		t.Errorf("the own branch did not reach the remote: %s", out)
	}
	if cp := read("checkpoint"); !strings.Contains(cp, "commit") || strings.Contains(cp, "istari:") {
		t.Errorf("istari checkpoint: %s", cp)
	}
	if files, err := exec.Command("git", "-C", filepath.Join(server, "acme", "web.git"), "ls-tree", "--name-only", "istari/5/add-limits").CombinedOutput(); err != nil || !strings.Contains(string(files), "feature.txt") {
		t.Errorf("the checkpoint did not push the work: %s %v", files, err)
	}
	if comments := gh.Issue(5).Comments; len(comments) == 0 || !strings.Contains(comments[len(comments)-1].Body, "**Checkpoint**") {
		t.Errorf("no checkpoint entry on the ticket: %+v", comments)
	}
	if strings.Contains(read("allenv"), gh.Token) {
		t.Error("the GitHub token is in the agent's environment")
	}
	if p := read("priority"); !strings.Contains(p, "token_forbidden") || !strings.Contains(p, "exit 1") {
		t.Errorf("an agent setting a priority: %s", p)
	}
	// After the run the token is dead.
	var token string
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(line, "ISTARI_TOKEN="); ok {
			token = v
		}
	}
	cmd := exec.Command(binary, "--config", d.config, "whoami")
	cmd.Env = append(os.Environ(), "ISTARI_TOKEN="+token)
	if msg, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(msg), "token_invalid") {
		t.Errorf("whoami after the run = %s, %v; want token_invalid", msg, err)
	}
}
