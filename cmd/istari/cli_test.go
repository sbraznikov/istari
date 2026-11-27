package main_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
)

// cli runs istari against the daemon d.
func (d *daemon) cli(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return run(t, append([]string{"--config", d.config}, args...)...)
}

// mustCLI runs istari --json and decodes its output into out.
func (d *daemon) mustCLI(t *testing.T, out any, args ...string) {
	t.Helper()
	stdout, stderr, code := d.cli(t, append([]string{"--json"}, args...)...)
	if code != 0 {
		t.Fatalf("istari %v: exit %d: %s", args, code, stderr)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(stdout), out); err != nil {
			t.Fatalf("istari %v: %v in %q", args, err, stdout)
		}
	}
}

func (d *daemon) createTask(t *testing.T, id, role string) {
	t.Helper()
	d.call(t, "task.create", map[string]any{"actor": "system", "id": id, "ticket": "gh:acme/web#1", "project": "web", "role": role}, nil)
}

type lease struct {
	Task, Owner, Token string
}

func TestCommandsPrintATableByDefaultAndJSONWithTheFlag(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.createTask(t, "t_1", "implementer")
	createTicket(t, d, "gh:acme/web#1")
	for _, tc := range []struct {
		args   []string
		header []string
	}{
		{[]string{"task", "list"}, []string{"ID", "STATE", "ROLE", "TICKET"}},
		{[]string{"task", "show", "t_1"}, []string{"ID", "STATE"}},
		{[]string{"ticket", "list"}, []string{"TICKET", "STATE", "HOLD"}},
		{[]string{"ticket", "show", "gh:acme/web#1"}, []string{"TICKET", "STATE"}},
		{[]string{"tail"}, []string{"CURSOR", "TYPE", "ACTOR"}},
	} {
		out, stderr, code := d.cli(t, tc.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, stderr)
		}
		for _, h := range tc.header {
			if !strings.Contains(strings.SplitN(out, "\n", 2)[0], h) {
				t.Errorf("%v: first line %q lacks column %s", tc.args, strings.SplitN(out, "\n", 2)[0], h)
			}
		}
		jsonOut, _, code := d.cli(t, append(tc.args, "--json")...)
		if code != 0 || !json.Valid([]byte(jsonOut)) {
			t.Errorf("%v --json: exit %d, output is not JSON: %q", tc.args, code, jsonOut)
		}
	}
	var tasks []struct{ ID, State string }
	d.mustCLI(t, &tasks, "task", "list")
	if len(tasks) != 1 || tasks[0].ID != "t_1" || tasks[0].State != "ready" {
		t.Fatalf("task list --json = %+v", tasks)
	}
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.createTask(t, "t_1", "implementer")
	for _, tc := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"task", "show", "t_404"}, 1, "not_found"},
		{[]string{"task", "claim"}, 2, "--ready"},                        // nothing to claim named
		{[]string{"task", "renew", "t_1"}, 2, "--token"},                 // a lease action without the lease
		{[]string{"ticket", "pause"}, 2, "accepts 1 arg"},                // missing ref
		{[]string{"wait", "colour=red", "--timeout", "1s"}, 2, "colour"}, // unknown filter key
		{[]string{"wait", "type=task.completed", "--timeout", "300ms"}, 1, "timeout"},
	} {
		_, stderr, code := d.cli(t, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.says) {
			t.Errorf("istari %v = exit %d %q, want exit %d mentioning %q", tc.args, code, stderr, tc.code, tc.says)
		}
	}
	// Without a daemon the CLI fails, not hangs.
	_, stderr, code := run(t, "--config", writeDaemonConfig(t, newDataDir(t)), "task", "list")
	if code != 1 || !strings.Contains(stderr, "daemon") {
		t.Errorf("without a daemon: exit %d %q, want 1 mentioning the daemon", code, stderr)
	}
}

func TestATaskLifecycleThroughTheCLI(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.createTask(t, "t_1", "implementer")
	var l lease
	d.mustCLI(t, &l, "task", "claim", "t_1", "--actor", "agent:implementer-1")
	if l.Task != "t_1" || l.Owner != "agent:implementer-1" || l.Token == "" {
		t.Fatalf("claim = %+v", l)
	}
	owner := []string{"--actor", l.Owner, "--token", l.Token}
	d.mustCLI(t, nil, append([]string{"task", "renew", "t_1"}, owner...)...)
	d.mustCLI(t, nil, append([]string{"task", "block", "t_1", "--kind", "ci_red"}, owner...)...)
	d.mustCLI(t, nil, "task", "unblock", "t_1")
	d.mustCLI(t, &l, "task", "claim", "--ready", "--role", "implementer", "--actor", "agent:implementer-1")
	owner = []string{"--actor", l.Owner, "--token", l.Token}
	d.mustCLI(t, nil, append([]string{"task", "release", "t_1"}, owner...)...)
	d.mustCLI(t, &l, "task", "claim", "t_1", "--actor", "agent:implementer-1")
	owner = []string{"--actor", l.Owner, "--token", l.Token}
	d.mustCLI(t, nil, append([]string{"task", "fail", "t_1", "--reason", "tests red"}, owner...)...)
	d.mustCLI(t, &l, "task", "claim", "t_1", "--actor", "agent:implementer-1")
	d.mustCLI(t, nil, "task", "complete", "t_1", "--actor", l.Owner, "--token", l.Token)
	var task struct {
		State    string
		Attempts int
	}
	d.mustCLI(t, &task, "task", "show", "t_1")
	if task.State != "completed" || task.Attempts != 1 {
		t.Fatalf("t_1 = %+v, want completed after one failed attempt", task)
	}
	// The old token no longer works.
	_, stderr, code := d.cli(t, "task", "renew", "t_1", "--actor", l.Owner, "--token", l.Token)
	if code != 1 || !strings.Contains(stderr, "claim_lost") {
		t.Fatalf("renew after complete: exit %d %q", code, stderr)
	}
}

func TestTicketCommands(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	ref := "gh:acme/web#7"
	createTicket(t, d, ref)
	state := func() (string, string) {
		var tk struct{ State, Hold string }
		d.mustCLI(t, &tk, "ticket", "show", ref)
		return tk.State, tk.Hold
	}
	d.mustCLI(t, nil, "ticket", "pause", ref)
	if s, h := state(); s != "new" || h != "paused" {
		t.Fatalf("after pause: %s/%s", s, h)
	}
	if _, stderr, code := d.cli(t, "ticket", "retry", ref); code != 1 || !strings.Contains(stderr, "invalid_state") {
		t.Fatalf("retry of a paused ticket: exit %d %q, want refusal", code, stderr)
	}
	d.mustCLI(t, nil, "ticket", "resume", ref)
	if s, h := state(); s != "new" || h != "" {
		t.Fatalf("after resume: %s/%s", s, h)
	}
	d.mustCLI(t, nil, "ticket", "cancel", ref)
	d.mustCLI(t, nil, "ticket", "reopen", ref)
	if s, _ := state(); s != "new" {
		t.Fatalf("after cancel and reopen: %s", s)
	}
}

func TestUnholdLiftsOnlyABudgetHold(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	// The ticket of createTask's tasks.
	ref := "gh:acme/web#1"
	createTicket(t, d, ref)
	hold := func() string {
		var tk struct{ Hold string }
		d.mustCLI(t, &tk, "ticket", "show", ref)
		return tk.Hold
	}
	d.mustCLI(t, nil, "ticket", "pause", ref)
	if _, stderr, code := d.cli(t, "ticket", "unhold", ref); code != 1 || !strings.Contains(stderr, "istari ticket resume") {
		t.Fatalf("unhold of a paused ticket: exit %d %q, want a refusal naming istari ticket resume", code, stderr)
	}
	if h := hold(); h != "paused" {
		t.Fatalf("after the refused unhold the hold is %q, want paused", h)
	}
	d.mustCLI(t, nil, "ticket", "resume", ref)

	d.createTask(t, "t_1", "implementer")
	var l lease
	d.mustCLI(t, &l, "task", "claim", "t_1", "--actor", "agent:a")
	d.mustCLI(t, nil, "task", "block", "t_1", "--actor", "agent:a", "--token", l.Token, "--kind", "budget_exceeded")
	d.call(t, "ticket.hold", map[string]any{"actor": "system", "ref": ref, "reason": "budget_exceeded"}, nil)
	if _, stderr, code := d.cli(t, "ticket", "unhold", ref, "--actor", "agent:a"); code != 1 || !strings.Contains(stderr, "human_only") {
		t.Fatalf("unhold by an agent: exit %d %q, want human_only", code, stderr)
	}
	d.mustCLI(t, nil, "ticket", "unhold", ref) // no runner here: no budget counts as spent
	if h := hold(); h != "" {
		t.Fatalf("after unhold the hold is %q, want none", h)
	}
	var task struct{ State string }
	d.mustCLI(t, &task, "task", "show", "t_1")
	if task.State != "ready" {
		t.Fatalf("after unhold t_1 is %s, want ready", task.State)
	}
}

func TestRetryRestartsTheExhaustedTasksAndLiftsTheHold(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	ref := "gh:acme/web#1"
	createTicket(t, d, ref)
	d.createTask(t, "t_1", "implementer")
	for range 3 {
		var l lease
		d.mustCLI(t, &l, "task", "claim", "t_1", "--actor", "agent:a")
		d.mustCLI(t, nil, "task", "fail", "t_1", "--actor", "agent:a", "--token", l.Token, "--reason", "red")
	}
	d.call(t, "ticket.hold", map[string]any{"actor": "system", "ref": ref, "reason": "attempts_exhausted"}, nil)
	d.mustCLI(t, nil, "ticket", "retry", ref)
	var task struct {
		State    string
		Attempts int
	}
	d.mustCLI(t, &task, "task", "show", "t_1")
	if task.State != "ready" || task.Attempts != 0 {
		t.Fatalf("after retry t_1 = %+v, want ready with no attempts", task)
	}
	var ticket struct{ Hold string }
	d.mustCLI(t, &ticket, "ticket", "show", ref)
	if ticket.Hold != "" {
		t.Fatalf("after retry the hold is %s", ticket.Hold)
	}
}

func TestATaskThatUsesUpItsAttemptsHoldsTheTicketUntilRetry(t *testing.T) {
	t.Parallel()
	ref := "gh:acme/web#5"
	d := startPipelineDaemon(t, ref, nil)
	d.step(t, "pipeline") // the plan task of the ticket in planning
	hold := func() string {
		var tk struct{ Hold string }
		d.mustCLI(t, &tk, "ticket", "show", ref)
		return tk.Hold
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); !ok(); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s\nstderr: %s", what, d.stderr)
			}
		}
	}
	// The daemon's own planner slot has no harness: it claims the plan task
	// and gives it back after each failed start, three times, then claims
	// nothing for a minute. The test claims by hand once the slot is paused.
	for deadline := time.Now().Add(90 * time.Second); !strings.Contains(d.stderr.String(), "runner planner-1: paused for"); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the planner slot did not pause\nstderr: %s", d.stderr)
		}
	}

	var l lease
	for range core.DefaultMaxAttempts {
		eventually("the plan task does not become ready", func() bool {
			out, _, code := d.cli(t, "--json", "task", "claim", "--ready", "--role", "planner", "--actor", "agent:planner-1")
			return code == 0 && json.Unmarshal([]byte(out), &l) == nil
		})
		if h := hold(); h != "" {
			t.Fatalf("hold = %q below the attempt limit, want none", h)
		}
		d.mustCLI(t, nil, "task", "fail", l.Task, "--actor", l.Owner, "--token", l.Token, "--reason", "red")
	}
	d.step(t, "pipeline") // the task out of attempts holds its ticket
	if h := hold(); h != "attempts_exhausted" {
		t.Fatalf("hold = %q after the last failed attempt, want attempts_exhausted", h)
	}

	d.mustCLI(t, nil, "ticket", "retry", ref)
	var task struct{ State string }
	d.mustCLI(t, &task, "task", "show", l.Task)
	if task.State != "ready" {
		t.Fatalf("after retry the plan task is %s, want ready", task.State)
	}
	if h := hold(); h != "" {
		t.Fatalf("after retry the hold is %q", h)
	}
	// The ticket goes on: its planner can claim the plan task again.
	eventually("the retried plan task cannot be claimed", func() bool {
		out, _, code := d.cli(t, "--json", "task", "claim", l.Task, "--actor", "agent:planner-1")
		return code == 0 && json.Unmarshal([]byte(out), &l) == nil
	})
	if h := hold(); h != "" {
		t.Fatalf("after the retried task was claimed the hold is %q", h)
	}
}

func TestRepeatingAWriteWithTheSameIdempotencyKeyPerformsItOnce(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.createTask(t, "t_1", "implementer")
	d.createTask(t, "t_2", "implementer")
	var first, second lease
	d.mustCLI(t, &first, "task", "claim", "--ready", "--actor", "agent:a", "--idempotency-key", "k-1")
	d.mustCLI(t, &second, "task", "claim", "--ready", "--actor", "agent:a", "--idempotency-key", "k-1")
	if first != second {
		t.Fatalf("repeated claim = %+v, want the first answer %+v", second, first)
	}
	var tasks []struct{ ID, State string }
	d.mustCLI(t, &tasks, "task", "list")
	claimed := 0
	for _, task := range tasks {
		if task.State == "claimed" {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("%d tasks claimed after a repeated claim, want 1", claimed)
	}
	var events []struct{ Type string }
	d.mustCLI(t, &events, "tail", "--type", "task.claimed")
	if len(events) != 1 {
		t.Fatalf("%d task.claimed events, want 1", len(events))
	}
	// The same key on another request is refused: another operation, another
	// actor (who must not receive agent:a's lease) or other arguments.
	for _, args := range [][]string{
		{"task", "unblock", "t_2", "--idempotency-key", "k-1"},
		{"task", "claim", "--ready", "--actor", "agent:b", "--idempotency-key", "k-1"},
		{"task", "claim", "t_2", "--actor", "agent:a", "--idempotency-key", "k-1"},
	} {
		out, stderr, code := d.cli(t, args...)
		if code != 1 || !strings.Contains(stderr, "idempotency_mismatch") || strings.Contains(out, first.Token) {
			t.Errorf("istari %v: exit %d %q %q, want idempotency_mismatch", args, code, stderr, out)
		}
	}
}

func TestTailAfterACursorResumesExactly(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	for i := range 5 {
		createTicket(t, d, fmt.Sprintf("gh:acme/web#%d", i))
	}
	var all []struct{ Cursor, Ticket string }
	d.mustCLI(t, &all, "tail")
	if len(all) != 5 {
		t.Fatalf("tail = %d events, want 5", len(all))
	}
	var rest []struct{ Cursor, Ticket string }
	d.mustCLI(t, &rest, "tail", "--after", all[1].Cursor)
	if len(rest) != 3 || rest[0].Cursor != all[2].Cursor || rest[2].Cursor != all[4].Cursor {
		t.Fatalf("tail --after %s = %+v, want the last three events", all[1].Cursor, rest)
	}
	var one []struct{ Ticket string }
	d.mustCLI(t, &one, "tail", "--ticket", "gh:acme/web#3")
	if len(one) != 1 || one[0].Ticket != "gh:acme/web#3" {
		t.Fatalf("tail --ticket = %+v", one)
	}
	// --follow keeps the stream open and delivers new events.
	cmd := exec.Command(binary, "--config", d.config, "--json", "tail", "--follow", "--after", all[4].Cursor)
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }() //nolint:errcheck // teardown: the daemon may have exited already, and Wait reports the kill
	time.Sleep(300 * time.Millisecond)
	createTicket(t, d, "gh:acme/web#99")
	dec := json.NewDecoder(stdout)
	var followed struct{ Ticket string }
	if err := dec.Decode(&followed); err != nil || followed.Ticket != "gh:acme/web#99" {
		t.Fatalf("tail --follow gave %+v, %v", followed, err)
	}
}

func TestWaitReturnsTheFirstMatchingEvent(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.createTask(t, "t_1", "implementer")
	d.createTask(t, "t_2", "implementer")
	var now []struct{ Cursor string }
	d.mustCLI(t, &now, "tail")
	cursor := now[len(now)-1].Cursor
	d.mustCLI(t, nil, "task", "claim", "t_2", "--actor", "agent:a") // matches type but not task
	done := make(chan string, 1)
	go func() {
		out, _, code := d.cli(t, "--json", "wait", "type=task.claimed", "task=t_1", "--after", cursor, "--timeout", "10s")
		done <- fmt.Sprintf("%d %s", code, out)
	}()
	d.mustCLI(t, nil, "task", "claim", "t_1", "--actor", "agent:a")
	got := <-done
	if !strings.HasPrefix(got, "0 ") || !strings.Contains(got, `"task.claimed"`) {
		t.Fatalf("wait = %s", got)
	}
}

func TestTwoShellsClaimingInALoopNeverGetTheSameTask(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t), "max_concurrent_runs = 100") //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome; 100 runs because the shells never complete
	const n = 40
	for i := range n {
		d.createTask(t, fmt.Sprintf("t_%02d", i), "implementer")
	}
	var mu sync.Mutex
	got := map[string][]string{}
	var wg sync.WaitGroup
	for shell := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The loop of one shell: claim until there is nothing left.
			script := fmt.Sprintf(`while out=$(%q --config %q --json task claim --ready --actor agent:shell-%d); do echo "$out"; done`, binary, d.config, shell)
			out, err := exec.Command("sh", "-c", script).Output()
			if err != nil && len(out) == 0 {
				t.Errorf("shell %d: %v", shell, err)
			}
			dec := json.NewDecoder(strings.NewReader(string(out)))
			for dec.More() {
				var l lease
				if err := dec.Decode(&l); err != nil {
					t.Errorf("shell %d: %v", shell, err)
					return
				}
				mu.Lock()
				got[l.Task] = append(got[l.Task], l.Owner)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != n {
		t.Fatalf("%d distinct tasks claimed, want %d", len(got), n)
	}
	for task, owners := range got {
		if len(owners) != 1 {
			t.Fatalf("task %s was claimed %d times: %v", task, len(owners), owners)
		}
	}
	owners := map[string]bool{}
	for _, o := range got {
		owners[o[0]] = true
	}
	if len(owners) < 2 {
		t.Logf("one shell claimed everything (%v); the test still holds but proves less", owners)
	}
}

func TestPauseAndResumeTheHandOut(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.createTask(t, "t_1", "implementer")
	d.createTask(t, "t_2", "implementer")
	var running lease
	d.mustCLI(t, &running, "task", "claim", "t_1", "--actor", "agent:a")
	d.mustCLI(t, nil, "pause", "--all")
	if _, stderr, code := d.cli(t, "task", "claim", "--ready", "--actor", "agent:b"); code != 1 || !strings.Contains(stderr, "paused") {
		t.Fatalf("claim while paused: exit %d %q", code, stderr)
	}
	d.mustCLI(t, nil, "task", "renew", "t_1", "--actor", running.Owner, "--token", running.Token) // running work goes on
	d.mustCLI(t, nil, "resume", "--all")
	d.mustCLI(t, nil, "task", "claim", "--ready", "--actor", "agent:b")
	if _, _, code := d.cli(t, "pause"); code != 2 {
		t.Fatalf("pause without a scope: exit %d, want 2", code)
	}
}

func TestMaxConcurrentRunsFromTheConfigBoundsClaims(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t), "max_concurrent_runs = 1") //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.createTask(t, "t_1", "implementer")
	d.createTask(t, "t_2", "implementer")
	d.mustCLI(t, nil, "task", "claim", "t_1", "--actor", "agent:a")
	if _, stderr, code := d.cli(t, "task", "claim", "t_2", "--actor", "agent:b"); code != 1 || !strings.Contains(stderr, "limit_reached") {
		t.Fatalf("second claim with max_concurrent_runs = 1: exit %d %q", code, stderr)
	}
}

func TestAScheduledActionRunsInTheDaemon(t *testing.T) {
	t.Parallel()
	dataDir := newDataDir(t)
	d := startSteppedDaemon(t, dataDir, writeDaemonConfig(t, dataDir))
	ref := "gh:acme/web#1"
	createTicket(t, d, ref)
	var timer struct{ ID, State string }
	d.mustCLI(t, &timer, "schedule", "--in", "1ms", "ticket.fire", `{"ref":"gh:acme/web#1","trigger":"cancel"}`)
	if timer.ID == "" || timer.State != "pending" {
		t.Fatalf("schedule = %+v", timer)
	}
	d.step(t, "scheduler")
	var fired []struct{ Type string }
	d.mustCLI(t, &fired, "tail", "--type", "timer.fired")
	if len(fired) != 1 {
		t.Fatalf("after the scheduler step %d timers fired, want 1", len(fired))
	}
	var tk struct{ State string }
	d.mustCLI(t, &tk, "ticket", "show", ref)
	if tk.State != "canceled" {
		t.Fatalf("after the scheduled cancel the ticket is %s", tk.State)
	}
	if _, stderr, code := d.cli(t, "schedule", "--in", "1s", "task.list"); code != 1 || !strings.Contains(stderr, "not a write operation") {
		t.Fatalf("scheduling a read: exit %d %q", code, stderr)
	}
}

func TestPausingOneProjectLeavesTheOthersToClaimReady(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
	d.call(t, "task.create", map[string]any{"actor": "system", "id": "a_web", "ticket": "gh:acme/web#1", "project": "web", "role": "implementer"}, nil)
	d.call(t, "task.create", map[string]any{"actor": "system", "id": "b_api", "ticket": "gh:acme/api#1", "project": "api", "role": "implementer"}, nil)
	d.mustCLI(t, nil, "pause", "--project", "web")
	var l lease
	d.mustCLI(t, &l, "task", "claim", "--ready", "--actor", "agent:a")
	if l.Task != "b_api" {
		t.Fatalf("claim --ready with web paused took %s, want b_api", l.Task)
	}
}

func TestRepeatingAScheduleWithItsKeyReturnsTheSameTimer(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project, and its timers are due in an hour: no loop pass decides the outcome
	var first, second struct{ ID, State string }
	d.mustCLI(t, &first, "schedule", "--in", "1h", "ticket.create", `{"ref":"gh:acme/web#5"}`, "--idempotency-key", "s-1")
	d.mustCLI(t, &second, "schedule", "--in", "1h", "ticket.create", `{"ref":"gh:acme/web#5"}`, "--idempotency-key", "s-1")
	if first.ID == "" || second != first {
		t.Fatalf("repeated schedule = %+v, want %+v", second, first)
	}
	if _, stderr, code := d.cli(t, "schedule", "--in", "1h", "ticket.fire", `{"ref":"x","colour":"red"}`); code != 1 || !strings.Contains(stderr, "colour") {
		t.Fatalf("a malformed scheduled request: exit %d %q, want a refusal naming the field", code, stderr)
	}
	// Keys of the form timer:<id> belong to the scheduler.
	resp, err := d.client.Do(func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://istari/v1/ticket.create", strings.NewReader(`{"actor":"human:alex","ref":"gh:acme/web#6"}`))
		r.Header.Set("Idempotency-Key", "timer:tm_1")
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a client key timer:… = %s, want 400", resp.Status)
	}
}

func TestProjectAndRoleLimitsFromTheConfig(t *testing.T) {
	t.Parallel()
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf(`[daemon]
data_dir = %q

[defaults.max_concurrent_runs_by_role]
reviewer = 1

[[project]]
name = "web"
max_concurrent_runs = 1

[[project]]
name = "api"
`, dataDir)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config) //nolint:forbidigo // projects without a repo and tasks made by hand: the claims meet the limits, no loop pass
	for _, task := range []struct{ id, project, role string }{
		{"web_1", "web", "implementer"}, {"web_2", "web", "implementer"},
		{"rev_1", "api", "reviewer"}, {"rev_2", "api", "reviewer"}, {"api_1", "api", "implementer"},
	} {
		d.call(t, "task.create", map[string]any{"actor": "system", "id": task.id, "ticket": "gh:acme/" + task.project + "#1", "project": task.project, "role": task.role}, nil)
	}
	for _, step := range []struct {
		id      string
		refused bool
	}{
		{"web_1", false}, {"web_2", true}, // project web: 1
		{"rev_1", false}, {"rev_2", true}, // role reviewer: 1
		{"api_1", false}, // another project and role
	} {
		_, stderr, code := d.cli(t, "task", "claim", step.id, "--actor", "agent:a")
		switch {
		case step.refused && (code != 1 || !strings.Contains(stderr, "limit_reached")):
			t.Fatalf("claim %s: exit %d %q, want limit_reached", step.id, code, stderr)
		case !step.refused && code != 0:
			t.Fatalf("claim %s: exit %d %q, want success", step.id, code, stderr)
		}
	}
}
