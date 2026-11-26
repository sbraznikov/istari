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
	r := newRecovered(t, unrelatedAgent, "in_progress", func(gh *githubtest.Server, pr int) {
		gh.SetHeadSHA(pr, "h1")
		gh.SetChecks("h1", githubtest.Check{Name: "test", Status: "completed", Conclusion: "failure", Rerun: "success"})
	})
	r.restart(t) // the watch polls at once: it reads the red head
	for deadline := time.Now().Add(60 * time.Second); len(r.d.eventTypes(t, r.ref.String(), "vcs.rerun")) == 0; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			out, _ := os.ReadFile(r.log)
			t.Fatalf("no rerun after a minute\nagent log:\n%s\ndaemon:\n%s", out, r.d.stderr)
		}
	}
	r.restart(t) // and now the rerun's result
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var tk struct{ State string }
		r.d.mustCLI(t, &tk, "ticket", "show", r.ref.String())
		if tk.State == "in_review" {
			break
		}
		if time.Now().After(deadline) {
			out, _ := os.ReadFile(r.log)
			t.Fatalf("the ticket is %s after a minute, want in_review after a green rerun\nagent log:\n%s\ndaemon:\n%s", tk.State, out, r.d.stderr)
		}
	}
	if n := r.gh.Reruns(); n != 1 {
		t.Fatalf("the fake saw %d reruns, want 1", n)
	}
	if ended := endedRuns(t, r.d, r.log, "implementer", 1); ended[0].Reason != "completed: the known ETXTBSY flake" {
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
