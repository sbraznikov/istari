package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/exectest"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// askingRefiner logs its brief; on a brief without answers it asks two
// questions, else one more, so the ticket is held again and stays quiet.
const askingRefiner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = refiner ]; then
  printf 'BRIEF START\n%%s\nBRIEF END\n' "$*" >> "$log"
  case "$*" in
  *ANSWER-TWO*) "$istari" refine submit --file - >> "$log" 2>&1 <<'RESULT'
verdict: needs_input
size: S
questions:
  - Anything else?
RESULT
  ;;
  *) "$istari" refine submit --file - >> "$log" 2>&1 <<'RESULT'
verdict: needs_input
size: S
questions:
  - Which endpoints count?
  - Which unit is a quota in?
RESULT
  ;;
  esac
  "$istari" complete "asked" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAnswersInACommentAndFromTheCLILiftNeedsInputAndReachTheNextRefinersBrief(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	gh.SetPermission("mallory", "read")
	n := gh.Seed("Quotas", "We should add quotas.\n", "alex") // a trusted author's raw ticket: it is refined
	ref := fmt.Sprintf("gh:%s#%d", githubtest.Repo, n)
	dataDir := newDataDir(t)
	log := filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	exectest.Write(t, filepath.Join(bin, "claude"), fmt.Sprintf(askingRefiner, binary, log))
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startSteppedDaemon(t, dataDir, config, "GITHUB_SERVER_URL=file://"+gitServer(t), "PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	d.step(t, "sync")     // the raw ticket
	d.step(t, "pipeline") // its refine task and run
	endedRuns(t, d, log, "refiner", 1)
	// A run ends before its task's outcome is recorded; the next refine
	// needs this one completed.
	for deadline := time.Now().Add(30 * time.Second); len(d.eventTypes(t, ref, "task.completed")) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the refine task did not complete\ndaemon:\n%s", d.stderr)
		}
	}
	hold := func() string {
		t.Helper()
		var tk struct{ Hold string }
		d.mustCLI(t, &tk, "ticket", "show", ref)
		return tk.Hold
	}
	if h := hold(); h != "needs_input" {
		out, _ := os.ReadFile(log)
		t.Fatalf("after the refiner's questions the hold is %q, want needs_input\nagent log:\n%s", h, out)
	}
	var second string
	for _, c := range gh.Issue(n).Comments {
		if m := regexp.MustCompile(`\*\*Question\*\* (\S+-q2)`).FindStringSubmatch(c.Body); m != nil {
			second = m[1]
		}
	}
	if second == "" {
		t.Fatalf("no second question in the ticket: %+v", gh.Issue(n).Comments)
	}

	gh.HumanComment(n, "mallory", "MALLORY: no quotas at all.")
	gh.HumanComment(n, "alex", "ANSWER-ONE: the public endpoints.")
	d.step(t, "sync")
	if h := hold(); h != "needs_input" {
		t.Fatalf("after one answer of two the hold is %q, want needs_input", h)
	}
	if _, stderr, code := d.cli(t, "ticket", "answer", "--actor", "agent:refiner-1", ref, "--question", second, "AGENT: requests."); code == 0 || !strings.Contains(stderr, "human_only") {
		t.Fatalf("an agent's answer: exit %d, %s; want human_only", code, stderr)
	}
	var answered struct{ Hold string }
	d.mustCLI(t, &answered, "ticket", "answer", ref, "--question", second, "ANSWER-TWO: requests per minute.")
	if answered.Hold != "" {
		t.Fatalf("after both answers the hold is %q, want none", answered.Hold)
	}
	d.step(t, "pipeline") // the check is repeated: a second refine
	endedRuns(t, d, log, "refiner", 2)
	out, _ := os.ReadFile(log)
	briefs := strings.Split(string(out), "BRIEF START")
	next := briefs[len(briefs)-1]
	for _, want := range []string{"Which endpoints count?", "ANSWER-ONE: the public endpoints.", "Which unit is a quota in?", "ANSWER-TWO: requests per minute."} {
		if !strings.Contains(next, want) {
			t.Errorf("the next refiner's brief lacks %q:\n%s", want, next)
		}
	}
	for _, not := range []string{"MALLORY", "AGENT: requests."} {
		if strings.Contains(string(out), not) {
			t.Errorf("a refiner's brief has %q, which is no answer", not)
		}
	}
}
