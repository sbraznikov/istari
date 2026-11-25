package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// correctingRefiner submits a result that is no YAML, then a corrected one.
const correctingRefiner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = refiner ]; then
  printf 'questions: [unclosed\n' | "$istari" refine submit --file - >> "$log" 2>&1
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

func TestARefineResultCorrectedInTheSameRunCompletesTheRefineTask(t *testing.T) {
	gh := githubtest.New(t)
	gh.Seed("Quotas", "We should add quotas.\n", "alex") // a trusted author's raw ticket: it is refined
	dataDir := newDataDir(t)
	log := filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(correctingRefiner, binary, log)), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+gitServer(t), "PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	d.mustCLI(t, nil, "sync")

	runs := endedRuns(t, d, log, "refiner", 1)
	out, _ := os.ReadFile(log)
	if !strings.Contains(string(out), "yaml") {
		t.Fatalf("the first result was not refused:\n%s", out)
	}
	if runs[0].Reason != "completed: refined" {
		t.Fatalf("the refiner run ended %q, want it completed: its refused result was corrected\nagent log:\n%s", runs[0].Reason, out)
	}
	if task := showTask(t, d, runs[0].Task); task.State != "completed" {
		t.Fatalf("the refine task is %s, want completed", task.State)
	}
}
