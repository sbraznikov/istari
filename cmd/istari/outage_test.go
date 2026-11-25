package main_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// onceRefiner submits one result and completes; a later run of it waits, so
// that a test sees what the first run left.
const onceRefiner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = refiner ]; then
  if [ -e "$log.ran" ]; then sleep 60; exit 0; fi
  : > "$log.ran"
  "$istari" refine submit --file - >> "$log" 2>&1 <<'RESULT'
verdict: needs_input
size: S
questions:
  - Which endpoints count?
RESULT
  "$istari" complete "refined" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

// refineDuringOutage starts a daemon whose refiner's "Refined" comment is
// answered status n times, and returns the daemon, the issue and the agent log.
func refineDuringOutage(t *testing.T, n, status int) (*daemon, *githubtest.Server, int, string) {
	t.Helper()
	gh := githubtest.New(t)
	issue := gh.Seed("Quotas", "We should add quotas.\n", "alex")
	gh.FailComments("**Refined**", n, status)
	dataDir := newDataDir(t)
	log := filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(onceRefiner, binary, log)), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+gitServer(t), "PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	d.mustCLI(t, nil, "sync")
	return d, gh, issue, log
}

func refinedComments(gh *githubtest.Server, n int) int {
	count := 0
	for _, c := range gh.Issue(n).Comments {
		if strings.HasPrefix(c.Body, "**Refined**") {
			count++
		}
	}
	return count
}

func TestARefineResultIsAcceptedThroughAShortServerOutage(t *testing.T) {
	t.Parallel()
	d, gh, issue, log := refineDuringOutage(t, 2, http.StatusInternalServerError)
	runs := endedRuns(t, d, log, "refiner", 1)
	out, _ := os.ReadFile(log)
	if runs[0].Reason != "completed: refined" {
		t.Fatalf("the refiner run ended %q, want completed on its first submit\nagent log:\n%s", runs[0].Reason, out)
	}
	if task := showTask(t, d, runs[0].Task); task.State != "completed" || task.Attempts != 0 {
		t.Fatalf("the refine task is %+v, want completed with no failed attempt", task)
	}
	if got := refinedComments(gh, issue); got != 1 {
		t.Fatalf("%d refined comments, want 1", got)
	}
}

func TestARefineResultThatOutlastsTheRetriesCostsNoAttemptAndItsCommentFollows(t *testing.T) {
	t.Parallel()
	d, gh, issue, log := refineDuringOutage(t, 4, http.StatusServiceUnavailable) // the call and its three retries
	runs := endedRuns(t, d, log, "refiner", 1)
	out, _ := os.ReadFile(log)
	if !strings.HasPrefix(runs[0].Reason, "yielded: ") {
		t.Fatalf("the refiner run ended %q, want it yielded, not rejected\nagent log:\n%s", runs[0].Reason, out)
	}
	if task := showTask(t, d, runs[0].Task); task.Attempts != 0 || task.State == "failed" {
		t.Fatalf("the refine task is %+v, want it handed out again with no failed attempt", task)
	}
	if got := refinedComments(gh, issue); got != 0 {
		t.Fatalf("%d refined comments during the outage", got)
	}
	d.mustCLI(t, nil, "sync") // the tracker answers again: the daemon delivers what is pending
	for deadline := time.Now().Add(10 * time.Second); refinedComments(gh, issue) == 0 && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
	}
	if got := refinedComments(gh, issue); got != 1 {
		t.Fatalf("%d refined comments once the tracker answers, want 1", got)
	}
}

func TestARefusalOfTheTrackerStillRefusesTheRefineResult(t *testing.T) {
	t.Parallel()
	d, _, _, log := refineDuringOutage(t, 1, http.StatusUnprocessableEntity)
	runs := endedRuns(t, d, log, "refiner", 1)
	out, _ := os.ReadFile(log)
	if !strings.HasPrefix(runs[0].Reason, "rejected: ") || !strings.Contains(string(out), "422") {
		t.Fatalf("the refiner run ended %q, want it rejected with the 422\nagent log:\n%s", runs[0].Reason, out)
	}
}
