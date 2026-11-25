package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// stageAgent plays each role of the pipeline: the planner submits a plan,
// the implementer writes, checkpoints and opens the pull request.
const stageAgent = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
echo "$ISTARI_ROLE $ISTARI_TASK" >> "$log"
case "$ISTARI_ROLE" in
planner)
  "$istari" plan submit --file - >> "$log" 2>&1 <<'PLAN'
summary: Write the feature file
slices: [the feature file]
seams: [{name: feature.txt, catches: its presence, misses: its content}]
zone: [feature.txt (new)]
PLAN
  ;;
implementer)
  echo "the feature" > feature.txt
  "$istari" checkpoint "The feature file; next the pull request" >> "$log" 2>&1
  "$istari" pr open --title "Add the feature" --body-file - >> "$log" 2>&1 <<'BODY'
## Summary

The feature file.

## Evidence

- Before: none. After: feature.txt.

## Merge Danger

**Door:** two-way

**Blast Radius:** none
BODY
  ;;
esac
"$istari" complete "done" >> "$log" 2>&1
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAReadyTicketBecomesADraftPullRequest(t *testing.T) {
	t.Parallel()
	server := gitServer(t)
	seed := filepath.Join(server, "seed")
	if err := os.MkdirAll(filepath.Join(seed, ".istari"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".istari", "pipeline.toml"), []byte("[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \"test -f feature.txt\"\nverify_red = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "pipeline")
	git(t, seed, "push", "-q", "origin", "main")

	gh := githubtest.New(t)
	gh.SetFile("go.mod", "module x\n")
	n := gh.Seed("Add rate limits", strings.SplitN(conforming, "\n", 2)[1], "alex") // alex may write: trusted
	dataDir := newDataDir(t)
	log := filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(stageAgent, binary, log)), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server,
		"PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if pr := findPull(gh); pr > 0 {
			got := gh.Issue(pr)
			if !got.Draft || !strings.Contains(got.Body, fmt.Sprintf("Closes #%d", n)) || !strings.HasPrefix(got.Branch, fmt.Sprintf("istari/%d/", n)) {
				t.Fatalf("pull request = %+v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			out, _ := os.ReadFile(log)
			t.Fatalf("no pull request after a minute\nagent log:\n%s\ndaemon:\n%s", out, d.stderr)
		}
	}
}

func findPull(gh *githubtest.Server) int {
	for n := 1; n <= gh.IssueCount(); n++ {
		if gh.Issue(n).PullRequest {
			return n
		}
	}
	return 0
}
