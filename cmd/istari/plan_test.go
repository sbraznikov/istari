package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	md := filepath.Join(t.TempDir(), "one.md")
	os.WriteFile(md, []byte("# A single ticket\n\n## Context\n\nOne.\n"), 0o644)
	d.mustCLI(t, &refs, "ticket", "create", "--file", md)
	if gh.IssueCount() != 3 || gh.Issue(3).Title != "A single ticket" {
		t.Fatalf("a single ticket: %d issues, %+v", gh.IssueCount(), gh.Issue(3))
	}
}

// longPlanner writes a plan of more than 20,000 characters to its result
// file and submits it without --file.
const longPlanner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = planner ]; then
  {
    echo "summary: Write the feature file"
    echo "slices:"
    i=0
    while [ $i -lt 300 ]; do
      echo "  - \"slice $i: the feature file grows by one line; its test reads | the line; then the next\""
      i=$((i+1))
    done
    echo "seams: [{name: feature.txt, catches: its presence, misses: its content}]"
    echo "zone: [feature.txt (new)]"
  } > "$ISTARI_RESULT"
  echo "bytes $(wc -c < "$ISTARI_RESULT")" >> "$log"
  "$istari" plan submit >> "$log" 2>&1
  echo "submit exit $?" >> "$log"
  "$istari" complete "planned" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAPlannerSubmitsAPlanOf20000CharactersThroughItsResultFile(t *testing.T) {
	t.Parallel()
	server := gitServer(t)
	gh := githubtest.New(t)
	gh.SetFile("go.mod", "module x\n")
	n := gh.Seed("Add rate limits", strings.SplitN(conforming, "\n", 2)[1], "alex")
	dataDir := newDataDir(t)
	log := filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(longPlanner, binary, log)), 0o755); err != nil {
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
		out, _ := os.ReadFile(log)
		if strings.Contains(string(out), "submit exit") {
			var bytes int
			fmt.Sscanf(string(out), "bytes %d", &bytes)
			if bytes < 20000 || !strings.Contains(string(out), "submit exit 0") {
				t.Fatalf("a plan of %d bytes from the result file was not accepted:\n%s", bytes, out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no plan submitted after a minute\nagent log:\n%s\ndaemon:\n%s", out, d.stderr)
		}
	}
	comments := gh.Issue(n).Comments
	if !slices.ContainsFunc(comments, func(c githubtest.Comment) bool { return strings.Contains(c.Body, "slice 299: the feature file grows") }) {
		t.Fatalf("the journal has no plan with all its slices: %d comments", len(comments))
	}
}

func TestPlanSubmitWithoutAFileOrAResultFileSaysWhatIsMissing(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	_, stderr, code := d.cli(t, "plan", "submit")
	if code == 0 || !strings.Contains(stderr, "ISTARI_RESULT") {
		t.Fatalf("plan submit without --file: exit %d %q, want an error naming ISTARI_RESULT", code, stderr)
	}
}

func TestRefineAndReviewSubmitReadTheResultFileWithoutAFile(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	for _, role := range []string{"refine", "review"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			result := filepath.Join(t.TempDir(), "result.yaml")
			submit := func() ([]byte, error) {
				cmd := exec.Command(binary, "--config", d.config, role, "submit")
				cmd.Env = append(os.Environ(), "ISTARI_RESULT="+result)
				return cmd.CombinedOutput()
			}
			if out, err := submit(); err == nil || !strings.Contains(string(out), "write the result there first") {
				t.Fatalf("%s submit before the result file is written: %v\n%s", role, err, out)
			}
			if err := os.WriteFile(result, []byte("verdict: ok\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := submit()
			// Read and sent: the daemon answers that the result is a role's.
			if err == nil || !strings.Contains(string(out), "token_invalid") {
				t.Fatalf("%s submit from the result file: %v\n%s", role, err, out)
			}
		})
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
