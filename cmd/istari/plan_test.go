package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

func TestTicketCreateMakesAPlanOnceAndDepsPrintsItsTree(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "PATH="+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	plan := filepath.Join(t.TempDir(), "plan.yaml")
	os.WriteFile(plan, []byte("tickets:\n  - key: base\n    title: The base\n    body: \"## Context\\n\\nFirst.\\n\"\n  - key: top\n    title: On top\n    needs: [base]\n    body: \"## Context\\n\\nNeeds #base.\\n\"\n"), 0o644)
	var refs map[string]string
	for range 2 { // the same key twice: one plan
		d.mustCLI(t, &refs, "ticket", "create", "--file", plan, "--idempotency-key", "plan-1")
	}
	if gh.IssueCount() != 2 || refs["base"] == "" || refs["top"] == "" {
		t.Fatalf("issues %d, refs %v; want two tickets once", gh.IssueCount(), refs)
	}
	out, stderr, code := d.cli(t, "ticket", "deps")
	if code != 0 || !strings.Contains(out, refs["top"]+"  new") || !strings.Contains(out, "needs "+refs["base"]+"  new") {
		t.Fatalf("ticket deps: exit %d %s\n%s", code, stderr, out)
	}
	other := filepath.Join(t.TempDir(), "other.yaml")
	os.WriteFile(other, []byte("tickets:\n  - key: away\n    title: Elsewhere\n    repo: acme/other\n"), 0o644)
	if _, stderr, code := d.cli(t, "ticket", "create", "--file", other); code == 0 || !strings.Contains(stderr, "acme/other is no repository istari syncs") || gh.IssueCount() != 2 {
		t.Fatalf("a ticket in a repository istari does not sync: exit %d, %d issues\n%s", code, gh.IssueCount(), stderr)
	}
	md := filepath.Join(t.TempDir(), "one.md")
	os.WriteFile(md, []byte("# A single ticket\n\n## Context\n\nOne.\n"), 0o644)
	d.mustCLI(t, &refs, "ticket", "create", "--file", md)
	if gh.IssueCount() != 3 || gh.Issue(3).Title != "A single ticket" {
		t.Fatalf("a single ticket: %d issues, %+v", gh.IssueCount(), gh.Issue(3))
	}
}

// splitAgent is a planner that submits a split whose child has no size,
// then the same split with the size, in one run.
const splitAgent = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
[ "$ISTARI_ROLE" = planner ] || exit 1
for size in "" "size: S"; do
  echo "--- submit" >> "$log"
  "$istari" plan submit --file - >> "$log" 2>&1 <<PLAN
summary: Split in one
slices: [the half]
seams: [{name: half, catches: its presence, misses: its content}]
zone: [half.txt (new)]
split:
  tickets:
    - key: half
      title: The half
      type: feature
      $size
      body: |
        ## Context

        Half of it.

        ## What needs to be done

        - The half.

        ## Acceptance criteria

        - [ ] It is there.

        ## Out of scope

        - The other half.

        ## Dependencies

        - Needs: none
PLAN
done
"$istari" complete "done" >> "$log" 2>&1
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestARefusedSplitIsFixedAndResubmittedInTheSameRun(t *testing.T) {
	t.Parallel()
	server := gitServer(t)
	gh := githubtest.New(t)
	gh.SetFile("go.mod", "module x\n")
	n := gh.Seed("Add rate limits", strings.SplitN(conforming, "\n", 2)[1], "alex")
	dataDir := newDataDir(t)
	log := filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(splitAgent, binary, log)), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server,
		"PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	for deadline := time.Now().Add(60 * time.Second); gh.IssueCount() < 2; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			out, _ := os.ReadFile(log)
			t.Fatalf("no child after a minute\nagent log:\n%s\ndaemon:\n%s", out, d.stderr)
		}
	}
	out, _ := os.ReadFile(log)
	// ended is how each run ended and the daemon's log, for a failure.
	ended := func() string {
		var runs []struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
		}
		d.mustCLI(t, &runs, "run", "list")
		return fmt.Sprintf("runs: %+v\ndaemon:\n%s", runs, d.stderr)
	}
	submits := strings.Split(string(out), "--- submit")
	if len(submits) != 3 || !strings.Contains(submits[1], "child half") || !strings.Contains(submits[1], "no size") {
		t.Fatalf("the first submission's answer:\n%s\nwant the child's key and its missing size\n%s", out, ended())
	}
	if gh.IssueCount() != 2 || !strings.HasSuffix(gh.Issue(2).Body, "| feature | S | P2 | `"+githubtest.Repo+"` |\n") {
		t.Fatalf("issues %d, the child %+v; want one child with its table\n%s", gh.IssueCount(), gh.Issue(2), ended())
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var parent struct {
			State string `json:"state"`
		}
		d.mustCLI(t, &parent, "ticket", "show", fmt.Sprintf("gh:%s#%d", githubtest.Repo, n))
		var runs []struct {
			State    string `json:"state"`
			Rejected string `json:"rejected"`
		}
		d.mustCLI(t, &runs, "run", "list")
		if parent.State == "in_progress" && len(runs) == 1 && runs[0].State != "running" {
			if runs[0].Rejected != "" {
				t.Fatalf("the run = %+v, want it not rejected: the planner fixed the split in it", runs[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the parent is %s, want in_progress: the second plan split it\nagent log:\n%s\n%s", parent.State, out, ended())
		}
	}
}

func TestPlanSubmitReadsItsPlanFromStandardInput(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	cmd := exec.Command(binary, "--config", d.config, "plan", "submit", "--file", "-")
	cmd.Stdin = strings.NewReader("summary: s\n")
	out, err := cmd.CombinedOutput()
	// Read and sent: the daemon answers that a plan is the planner's.
	if err == nil || !strings.Contains(string(out), "token_invalid") {
		t.Fatalf("plan submit from stdin: %v\n%s", err, out)
	}
}
