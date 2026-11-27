package main_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// countingRefiner submits a result whose assumption and question name its
// run (run 1, run 2, ...) and completes only when the submit was taken.
const countingRefiner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = refiner ]; then
  n=$(( $(cat "$log.n" 2>/dev/null || echo 0) + 1 )); echo $n > "$log.n"
  if "$istari" refine submit --file - >> "$log" 2>&1 <<RESULT
verdict: needs_input
size: S
assumptions:
  - Endpoints are counted per key since run $n
questions:
  - Which endpoints count in run $n?
RESULT
  then "$istari" complete "refined" >> "$log" 2>&1; fi
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
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(countingRefiner, binary, log)), 0o755); err != nil {
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

// comments returns the bodies of the issue's comments that start with prefix.
func comments(gh *githubtest.Server, n int, prefix string) []string {
	var out []string
	for _, c := range gh.Issue(n).Comments {
		if strings.HasPrefix(c.Body, prefix) {
			out = append(out, c.Body)
		}
	}
	return out
}

// yieldedAndCompleted splits two ended runs into the one that yielded and
// the one that completed, failing the test unless there is one of each.
func yieldedAndCompleted(t *testing.T, runs []agentRun, log string) (yielded, completed agentRun) {
	t.Helper()
	for _, r := range runs {
		switch {
		case strings.HasPrefix(r.Reason, "yielded: "):
			yielded = r
		case strings.HasPrefix(r.Reason, "completed: "):
			completed = r
		}
	}
	if yielded.Task == "" || completed.Task == "" {
		out, _ := os.ReadFile(log)
		t.Fatalf("runs %+v, want one yielded by the outage and one completed after it\nagent log:\n%s", runs, out)
	}
	return yielded, completed
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
	if got := comments(gh, issue, "**Refined**"); len(got) != 1 {
		t.Fatalf("%d refined comments, want 1", len(got))
	}
}

func TestARefineResultThatOutlastsTheRetriesCostsNoAttemptAndIsRedoneByTheNextRun(t *testing.T) {
	t.Parallel()
	d, gh, issue, log := refineDuringOutage(t, 4, http.StatusServiceUnavailable) // the call and its three retries
	yielded, completed := yieldedAndCompleted(t, endedRuns(t, d, log, "refiner", 2), log)
	out, _ := os.ReadFile(log)
	if !strings.Contains(string(out), "without istari complete") {
		t.Fatalf("the refusal does not tell the agent to end its run without istari complete\nagent log:\n%s", out)
	}
	if task := showTask(t, d, completed.Task); yielded.Task != completed.Task || task.Attempts != 0 {
		t.Fatalf("the refine task is %+v, want the yielded task handed out again with no failed attempt", task)
	}
	if logged := d.stderr.String(); !strings.Contains(logged, "comment on gh:") || !strings.Contains(logged, "tried again when task "+yielded.Task+" is handed out next") {
		t.Fatalf("the daemon log does not name the failed write and when it is tried again:\n%s", logged)
	}
	d.mustCLI(t, nil, "sync") // the tracker answers: nothing of the first run follows
	if got := comments(gh, issue, "**Refined**"); len(got) != 1 {
		t.Fatalf("refined comments %q, want one", got)
	}
	all := strings.Join(comments(gh, issue, ""), "\n")
	if strings.Contains(all, "run 1") || !strings.Contains(all, "in run 2?") {
		t.Fatalf("the ticket's comments are\n%s\nwant the second run's questions and nothing of the first run", all)
	}
}

// outagePlanner submits a plan that names its run once the test has started
// the outage (the file $log.go), and completes only when it was taken.
const outagePlanner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = planner ]; then
  n=$(( $(cat "$log.n" 2>/dev/null || echo 0) + 1 )); echo $n > "$log.n"
  while [ ! -e "$log.go" ]; do sleep 0.1; done
  if "$istari" plan submit --file - >> "$log" 2>&1 <<PLAN
summary: Write the feature file in run $n
slices: [the feature file]
seams: [{name: feature.txt, catches: its presence, misses: its content}]
zone: [feature.txt (new)]
PLAN
  then "$istari" complete "planned" >> "$log" 2>&1; fi
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAPlanWhoseJournalEntryOutlastsTheRetriesCostsNoAttemptAndIsRedone(t *testing.T) {
	t.Parallel()
	d, gh, issue, log := pipelineDaemon(t, planOnlyPipeline, func(log string) string { return fmt.Sprintf(outagePlanner, binary, log) })
	gh.FailComments("**Plan**", 4, http.StatusBadGateway)
	if err := os.WriteFile(log+".go", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	yielded, completed := yieldedAndCompleted(t, endedRuns(t, d, log, "planner", 2), log)
	if task := showTask(t, d, completed.Task); yielded.Task != completed.Task || task.Attempts != 0 {
		t.Fatalf("the plan task is %+v, want the yielded task handed out again with no failed attempt", task)
	}
	d.mustCLI(t, nil, "sync")
	got := comments(gh, issue, "**Plan**")
	if len(got) != 1 || !strings.Contains(got[0], "run 2") {
		t.Fatalf("plan entries %q, want only the second run's", got)
	}
}

// outageReviewer submits a report that names its run once the test has
// started the outage, and completes only when it was taken.
const outageReviewer = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = reviewer ]; then
  n=$(( $(cat "$log.n" 2>/dev/null || echo 0) + 1 )); echo $n > "$log.n"
  while [ ! -e "$log.go" ]; do sleep 0.1; done
  if "$istari" review submit --file - >> "$log" 2>&1 <<REPORT
standards: 0 findings
spec: 1 finding, a burst is not refused
findings:
  - {axis: spec, kind: missing, file: feature.txt, line: 1, source: ticket, quote: A burst is refused., text: run $n finds that nothing refuses a burst}
REPORT
  then "$istari" complete "reviewed" >> "$log" 2>&1; fi
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAReviewTheCodeHostCannotTakeCostsNoAttemptAndIsRedone(t *testing.T) {
	t.Parallel()
	r := newReviewed(t, outageReviewer)
	r.gh.FailReviews(4, http.StatusInternalServerError) // the call and its three retries
	if err := os.WriteFile(r.log+".go", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	yielded, completed := yieldedAndCompleted(t, endedReviews(t, r.d, r.log, 2), r.log)
	if task := showTask(t, r.d, completed.Task); yielded.Task != completed.Task || task.Attempts != 0 {
		t.Fatalf("the review task is %+v, want the yielded task handed out again with no failed attempt", task)
	}
	r.d.mustCLI(t, nil, "sync")
	reviews := r.gh.Issue(r.pr).Reviews
	if len(reviews) != 1 || !strings.Contains(reviews[0].Body, "run 2") {
		t.Fatalf("reviews = %+v, want only the second run's on the pull request", reviews)
	}
	if got := comments(r.gh, r.ref.Number, "**Review**"); len(got) != 1 || !strings.Contains(got[0], "run 2") {
		t.Fatalf("review entries %q, want only the second run's in the journal", got)
	}
}

func TestARefusalOfTheTrackerStillRefusesTheRefineResult(t *testing.T) {
	t.Parallel()
	d, _, _, log := refineDuringOutage(t, 1, http.StatusUnprocessableEntity)
	// The task is handed out again after the rejection and the retry is
	// taken, and run list orders runs by their random ids: the rejected run
	// need not come first.
	runs := endedRuns(t, d, log, "refiner", 1)
	out, _ := os.ReadFile(log)
	rejected := slices.ContainsFunc(runs, func(r agentRun) bool { return strings.HasPrefix(r.Reason, "rejected: ") })
	if !rejected || !strings.Contains(string(out), "422") {
		t.Fatalf("the refiner runs ended %+v, want one rejected with the 422\nagent log:\n%s", runs, out)
	}
}
