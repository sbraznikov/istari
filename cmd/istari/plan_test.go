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

// correctingPlanner submits a plan that is no YAML, then a corrected one.
const correctingPlanner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = planner ]; then
  printf 'slices: [unclosed\n' | "$istari" plan submit --file - >> "$log" 2>&1
  "$istari" plan submit --file - >> "$log" 2>&1 <<'PLAN'
summary: Write the feature file
slices: [the feature file]
seams: [{name: feature.txt, catches: its presence, misses: its content}]
zone: [feature.txt (new)]
PLAN
  "$istari" complete "planned" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

const planOnlyPipeline = `[checks]
typecheck = "true"
test = "true"
full = "true"
verify_red = false

[[stage]]
name = "plan"
role = "planner"
`

func TestAPlanCorrectedInTheSameRunCompletesThePlanTask(t *testing.T) {
	t.Parallel()
	d, _, _, log := pipelineDaemon(t, planOnlyPipeline, func(log string) string { return fmt.Sprintf(correctingPlanner, binary, log) })
	runs := endedRuns(t, d, log, "planner", 1)
	out, _ := os.ReadFile(log)
	if !strings.Contains(string(out), "yaml") {
		t.Fatalf("the first plan was not refused:\n%s", out)
	}
	if runs[0].Reason != "completed: planned" {
		t.Fatalf("the planner run ended %q, want it completed: its refused plan was corrected\nagent log:\n%s", runs[0].Reason, out)
	}
	if task := showTask(t, d, runs[0].Task); task.State != "completed" {
		t.Fatalf("the plan task is %s, want completed", task.State)
	}
}

// filingPlanner files a ticket with istari ticket create, then ends its run.
const filingPlanner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = planner ]; then
  printf '# Found while planning\n\n## Context\n\nA gap.\n' > "$ISTARI_RESULT.md"
  "$istari" ticket create --file "$ISTARI_RESULT.md" >> "$log" 2>&1
  echo "create exit $?" >> "$log"
  "$istari" complete "filed" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestATicketAnAgentFilesIsCreatedByTheAgentInTail(t *testing.T) {
	t.Parallel()
	d, gh, _, log := pipelineDaemon(t, planOnlyPipeline, func(log string) string { return fmt.Sprintf(filingPlanner, binary, log) })
	endedRuns(t, d, log, "planner", 1)
	if out, _ := os.ReadFile(log); !strings.Contains(string(out), "create exit 0") || gh.IssueCount() != 2 {
		t.Fatalf("the planner's ticket create failed: %d issues\n%s", gh.IssueCount(), out)
	}
	filed := "gh:" + githubtest.Repo + "#2" // the token owner is its author on GitHub
	var events []struct {
		Actor   string
		Payload struct{ Trigger string }
	}
	d.mustCLI(t, &events, "tail", "--ticket", filed, "--type", "ticket.transitioned")
	if len(events) == 0 || events[0].Payload.Trigger != "create" || !strings.HasPrefix(events[0].Actor, "agent:planner") {
		t.Fatalf("transitions of %s = %+v, want create by the planner agent", filed, events)
	}
}

func TestTicketCreateTakesThePriorityOfTheTicketsTable(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "PATH="+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	md := filepath.Join(t.TempDir(), "ticket.md")
	os.WriteFile(md, []byte("# Rate limits\n\n## Context\n\nLimits protect the API.\n\n---\n\n| Type | Size | Priority | Repository |\n|---|---|---|---|\n| feature | S | P1 | `acme/web` |\n"), 0o644)
	var refs map[string]string
	d.mustCLI(t, &refs, "ticket", "create", "--file", md)
	var ref string
	for _, r := range refs {
		ref = r
	}
	var tk struct{ Priority string }
	d.mustCLI(t, &tk, "ticket", "show", ref)
	if tk.Priority != "P1" {
		t.Fatalf("priority of %s = %q, want P1 from its table", ref, tk.Priority)
	}
	if labels := gh.Issue(1).Labels; !slices.Contains(labels, "priority:P1") {
		t.Fatalf("labels = %v, want priority:P1", labels)
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

// waitingPlanner submits its plan when the test lets it: once while GitHub
// refuses for the rate limit, once after, then completes.
const waitingPlanner = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
plan() {
  "$istari" plan submit --file - >> "$log" 2>&1 <<'PLAN'
summary: Write the feature file
slices: [the feature file]
seams: [{name: feature.txt, catches: its presence, misses: its content}]
zone: [feature.txt (new)]
PLAN
}
if [ "$ISTARI_ROLE" = planner ]; then
  touch "$log.started"
  while [ ! -e "$log.refusing" ]; do sleep 0.05; done
  plan
  touch "$log.submitted"
  while [ ! -e "$log.answering" ]; do sleep 0.05; done
  plan
  "$istari" complete "planned" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAPlanSubmittedWhileGitHubRefusesForTheRateLimitIsLaterNotRejected(t *testing.T) {
	t.Parallel()
	d, gh, _, log := pipelineDaemon(t, planOnlyPipeline, func(log string) string { return fmt.Sprintf(waitingPlanner, binary, log) })
	// The planner is another process: the test polls its marker files.
	await := func(file string) {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if _, err := os.Stat(file); err == nil {
				return
			}
			if time.Now().After(deadline) {
				// An unreadable log is safe here: it only feeds this failure message.
				out, _ := os.ReadFile(log)
				t.Fatalf("the planner did not reach %s\nagent log:\n%s\ndaemon:\n%s", filepath.Base(file), out, d.stderr)
			}
		}
	}
	// signal lets the planner process move on.
	signal := func(file string) {
		t.Helper()
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	await(log + ".started")
	gh.SetRateLimit(0, time.Now().Add(2*time.Second))
	signal(log + ".refusing")
	await(log + ".submitted")
	gh.SetRateLimit(5000, time.Now().Add(time.Hour))
	signal(log + ".answering")
	runs := endedRuns(t, d, log, "planner", 1)
	out, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "rate_limited") || strings.Contains(string(out), "invalid_ticket") {
		t.Errorf("the plan submitted while GitHub refuses was not answered rate_limited:\n%s", out)
	}
	if runs[0].Reason != "completed: planned" {
		t.Fatalf("the planner run ended %q, want completed: planned\nagent log:\n%s", runs[0].Reason, out)
	}
}
