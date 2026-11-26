package main_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// unrelatedAgent is an implementer that answers the red CI of its pull
// request as not caused by the change, retrying until the watch has read
// the red head; and a reviewer that tries the same answer once.
const unrelatedAgent = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
case "$ISTARI_ROLE" in
implementer)
  i=0
  until "$istari" complete --ci-unrelated "the known ETXTBSY flake" >> "$log" 2>&1; do
    i=$((i+1)); [ $i -ge 100 ] && break; sleep 0.2
  done
  ;;
reviewer)
  "$istari" complete --ci-unrelated "flaky" >> "$log" 2>&1
  ;;
esac
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAnImplementersCIUnrelatedAnswerRerunsTheRedHeadOnce(t *testing.T) {
	t.Parallel()
	r := newSteppedRecovered(t, unrelatedAgent, "in_progress", func(gh *githubtest.Server, pr int) {
		gh.SetHeadSHA(pr, "h1")
		gh.SetChecks("h1", githubtest.Check{Name: "test", Status: "completed", Conclusion: "failure", Rerun: "success"})
	})
	r.d.step(t, "watch")    // the watch reads the red head
	r.d.step(t, "pipeline") // and the pipeline opens the implement task
	ended := endedRuns(t, r.d, r.log, "implementer", 1)
	// The run ends before its task does: the pipeline steps on the task's end.
	for deadline := time.Now().Add(30 * time.Second); len(r.d.eventTypes(t, r.ref.String(), "task.completed")) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the implement task did not complete\ndaemon:\n%s", r.d.stderr)
		}
	}
	r.d.step(t, "pipeline") // the completed task reruns the red head
	if events := r.d.eventTypes(t, r.ref.String(), "vcs.rerun"); len(events) != 1 {
		out, _ := os.ReadFile(r.log)
		t.Fatalf("%d reruns after the implementer's answer, want 1\nagent log:\n%s\ndaemon:\n%s", len(events), out, r.d.stderr)
	}
	r.d.step(t, "watch")    // the watch reads the green rerun
	r.d.step(t, "pipeline") // and the pipeline sends the ticket to review
	var tk struct{ State string }
	r.d.mustCLI(t, &tk, "ticket", "show", r.ref.String())
	if tk.State != "in_review" {
		out, _ := os.ReadFile(r.log)
		t.Fatalf("the ticket is %s, want in_review after a green rerun\nagent log:\n%s\ndaemon:\n%s", tk.State, out, r.d.stderr)
	}
	if n := r.gh.Reruns(); n != 1 {
		t.Fatalf("the fake saw %d reruns, want 1", n)
	}
	if ended[0].Reason != "completed: the known ETXTBSY flake" {
		t.Fatalf("the implementer run ended %q, want completed with its reason", ended[0].Reason)
	}
}

func TestOnlyAnImplementerMayAnswerCIUnrelated(t *testing.T) {
	t.Parallel()
	r := newReviewed(t, unrelatedAgent)
	endedRuns(t, r.d, r.log, "reviewer", 1)
	out, _ := os.ReadFile(r.log)
	if !strings.Contains(string(out), "only the implementer") {
		t.Fatalf("a reviewer's --ci-unrelated was not refused:\n%s", out)
	}
	if n := r.gh.Reruns(); n != 0 {
		t.Fatalf("the fake saw %d reruns, want none", n)
	}
}
