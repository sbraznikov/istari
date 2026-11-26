package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/exectest"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// stageAgent plays each role of the pipeline: the planner submits a plan,
// the implementer runs %[3]s, by default implementerOpens.
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
%[3]s
  ;;
esac
"$istari" complete "done" >> "$log" 2>&1
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

// implementerOpens writes, checkpoints and opens the pull request; a pr open
// that fails ends the run without istari complete, as the refusal says.
const implementerOpens = `  echo "the feature" > feature.txt
  "$istari" checkpoint "The feature file; next the pull request" >> "$log" 2>&1
  "$istari" pr open --title "Add the feature" --body-file - >> "$log" 2>&1 <<'BODY' || exit 0
## Summary

The feature file.

## Evidence

- Before: none. After: feature.txt.

## Merge Danger

**Door:** two-way

**Blast Radius:** none
BODY`

// implementFixture starts a daemon whose repository's full check is full and
// whose agent's implementer runs implementer; it returns the ticket's number
// and the agent's log.
func implementFixture(t *testing.T, full, implementer string) (d *daemon, gh *githubtest.Server, n int, log string) {
	t.Helper()
	server := gitServer(t)
	seed := filepath.Join(server, "seed")
	if err := os.MkdirAll(filepath.Join(seed, ".istari"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".istari", "pipeline.toml"), []byte("[checks]\ntypecheck = \"true\"\ntest = \"true\"\nfull = \""+full+"\"\nverify_red = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "pipeline")
	git(t, seed, "push", "-q", "origin", "main")

	gh = githubtest.New(t)
	gh.SetFile("go.mod", "module x\n")
	n = gh.Seed("Add rate limits", strings.SplitN(conforming, "\n", 2)[1], "alex") // alex may write: trusted
	dataDir := newDataDir(t)
	log = filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	exectest.Write(t, filepath.Join(bin, "claude"), fmt.Sprintf(stageAgent, binary, log, implementer))
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d = startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server,
		"PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	return d, gh, n, log
}

// waitFor polls cond for a minute and fails with the agent's log and the
// daemon's output when it does not come true.
func waitFor(t *testing.T, d *daemon, log, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); !cond(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			out, _ := os.ReadFile(log)
			t.Fatalf("no %s after a minute\nagent log:\n%s\ndaemon:\n%s", what, out, d.stderr)
		}
	}
}

func TestAReadyTicketBecomesADraftPullRequest(t *testing.T) {
	t.Parallel()
	d, gh, n, log := implementFixture(t, "test -f feature.txt", implementerOpens)
	waitFor(t, d, log, "pull request", func() bool { return findPull(gh) > 0 })
	got := gh.Issue(findPull(gh))
	if !got.Draft || !strings.Contains(got.Body, fmt.Sprintf("Closes #%d", n)) || !strings.HasPrefix(got.Branch, fmt.Sprintf("istari/%d/", n)) {
		t.Fatalf("pull request = %+v", got)
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

// implementerRuns returns the tasks of the implementer runs in the agent's log.
func implementerRuns(t *testing.T, log string) []string {
	t.Helper()
	data, _ := os.ReadFile(log)
	var tasks []string
	for line := range strings.Lines(string(data)) {
		if task, ok := strings.CutPrefix(strings.TrimSpace(line), "implementer "); ok {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func TestAnImplementersCompleteWithoutAPullRequestIsRefusedAndItsTaskReleased(t *testing.T) {
	t.Parallel()
	skipped := filepath.Join(t.TempDir(), "skipped")
	// The first run ends with istari complete and no pull request; the next
	// opens it.
	implementer := "  if [ ! -e " + skipped + " ]; then touch " + skipped + "; echo x > feature.txt; \"$istari\" checkpoint \"The feature file; next the pull request\" >> \"$log\" 2>&1\n  else\n" + implementerOpens + "\n  fi"
	d, gh, n, log := implementFixture(t, "test -f feature.txt", implementer)
	waitFor(t, d, log, "pull request", func() bool { return findPull(gh) > 0 })
	out, _ := os.ReadFile(log)
	if !strings.Contains(string(out), "no pull request") || !strings.Contains(string(out), "istari pr open") {
		t.Fatalf("the complete without a pull request was not refused naming istari pr open:\n%s", out)
	}
	if runs := implementerRuns(t, log); len(runs) < 2 || runs[0] != runs[1] {
		t.Fatalf("implementer runs of tasks %v, want the refused run's task run again", runs)
	}
	ref := fmt.Sprintf("gh:%s#%d", githubtest.Repo, n)
	if got := d.eventTypes(t, ref, "run.yielded"); len(got) != 1 {
		t.Fatalf("run.yielded events = %v, want the refused run's", got)
	}
}

func TestLiftingTheMainRedHoldStartsAnImplementRunThatOpensThePullRequest(t *testing.T) {
	t.Parallel()
	fixed := filepath.Join(t.TempDir(), "fixed")
	d, gh, n, log := implementFixture(t, "test -f "+fixed, implementerOpens)
	ref := fmt.Sprintf("gh:%s#%d", githubtest.Repo, n)
	hold := func() string {
		var tk struct{ Hold string }
		d.mustCLI(t, &tk, "ticket", "show", ref)
		return tk.Hold
	}
	waitFor(t, d, log, "needs_input hold", func() bool { return hold() == "needs_input" })
	if pr := findPull(gh); pr > 0 {
		t.Fatalf("pull request #%d opened while main is red", pr)
	}
	if err := os.WriteFile(fixed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d.mustCLI(t, nil, "ticket", "unhold", ref)
	waitFor(t, d, log, "pull request after the unhold", func() bool { return findPull(gh) > 0 })
	if runs := implementerRuns(t, log); len(runs) < 2 || runs[0] != runs[1] {
		t.Fatalf("implementer runs of tasks %v, want the held run's task run again", runs)
	}
	pr := findPull(gh)
	gh.SetHeadSHA(pr, "h1")
	gh.SetChecks("h1", githubtest.Check{Name: "test", Status: "completed", Conclusion: "success"})
	waitFor(t, d, log, "pull request marked ready by verify", func() bool { return !gh.Issue(pr).Draft })
}
