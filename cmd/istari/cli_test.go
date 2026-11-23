package main_test

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
	d := startDaemon(t, newDataDir(t))
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
	d := startDaemon(t, newDataDir(t))
	d.createTask(t, "t_1", "implementer")
	for _, tc := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"task", "show", "t_404"}, 1, "not_found"},
		{[]string{"task", "claim"}, 2, "--ready"},                        // nothing to claim named
		{[]string{"task", "renew", "t_1"}, 2, "--token"},                 // a lease action without the lease
		{[]string{"ticket", "pause"}, 2, "arg"},                          // missing ref
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
	d := startDaemon(t, newDataDir(t))
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
	d := startDaemon(t, newDataDir(t))
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
	d.call(t, "ticket.hold", map[string]any{"actor": "system", "ref": ref, "reason": "attempts_exhausted"}, nil)
	d.mustCLI(t, nil, "ticket", "retry", ref)
	if _, h := state(); h != "" {
		t.Fatalf("after retry: hold %s", h)
	}
}

func TestRepeatingAWriteWithTheSameIdempotencyKeyPerformsItOnce(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
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
	// The same key on a different operation is refused.
	if _, stderr, code := d.cli(t, "task", "unblock", "t_2", "--idempotency-key", "k-1"); code != 1 || !strings.Contains(stderr, "idempotency") {
		t.Fatalf("reused key on another operation: exit %d %q", code, stderr)
	}
}

func TestTailAfterACursorResumesExactly(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
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
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	time.Sleep(300 * time.Millisecond)
	createTicket(t, d, "gh:acme/web#99")
	dec := json.NewDecoder(stdout)
	var followed struct{ Ticket string }
	if err := dec.Decode(&followed); err != nil || followed.Ticket != "gh:acme/web#99" {
		t.Fatalf("tail --follow gave %+v, %v", followed, err)
	}
}

func TestWaitReturnsTheFirstMatchingEvent(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
	d.createTask(t, "t_1", "implementer")
	done := make(chan string, 1)
	go func() {
		out, _, code := d.cli(t, "--json", "wait", "type=task.claimed", "task=t_1", "--timeout", "10s")
		done <- fmt.Sprintf("%d %s", code, out)
	}()
	time.Sleep(300 * time.Millisecond)
	d.mustCLI(t, nil, "task", "claim", "t_1", "--actor", "agent:a")
	got := <-done
	if !strings.HasPrefix(got, "0 ") || !strings.Contains(got, `"task.claimed"`) {
		t.Fatalf("wait = %s", got)
	}
}

func TestTwoShellsClaimingInALoopNeverGetTheSameTask(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
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
	if !slices.Equal([]bool{owners["agent:shell-0"], owners["agent:shell-1"]}, []bool{true, true}) {
		t.Logf("one shell claimed everything (%v); the test still holds but proves less", owners)
	}
}
