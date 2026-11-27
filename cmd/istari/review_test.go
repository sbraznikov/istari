package main_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/exectest"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// reviewAgent is a reviewer that first tries to move the ticket itself, then
// submits a report with one finding on the Spec axis.
const reviewAgent = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = reviewer ]; then
  for last; do :; done
  printf '%%s\n' "$last" >> "$log"
  "$istari" ticket cancel "$ISTARI_TICKET" >> "$log" 2>&1
  "$istari" review submit --file - >> "$log" 2>&1 <<'REPORT'
standards: 0 findings
spec: 1 finding, a burst is not refused
findings:
  - {axis: spec, kind: missing, file: feature.txt, line: 1, source: ticket, quote: A burst is refused., text: nothing refuses a burst}
REPORT
  "$istari" complete "reviewed" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

// reviewed is a ticket recovered in in_review, with its branch, its pull
// request and a daemon whose claude is agent.
type reviewed struct {
	d   *daemon
	gh  *githubtest.Server
	ref tickets.Ref
	pr  int
	log string
}

func newReviewed(t *testing.T, agent string) reviewed {
	t.Helper()
	return newRecovered(t, agent, "in_review", nil)
}

// newRecovered is a ticket recovered in state, with its branch, its pull
// request, the fake GitHub set up by before ahead of the recover, and a
// daemon whose claude is agent.
func newRecovered(t *testing.T, agent, state string, before func(gh *githubtest.Server, pr int)) reviewed {
	t.Helper()
	return recoverWith(t, startDaemonWithConfig, agent, state, before)
}

// newSteppedRecovered is newRecovered on a stepped daemon: its PR watch and
// pipeline run one pass per step.
func newSteppedRecovered(t *testing.T, agent, state string, before func(gh *githubtest.Server, pr int)) reviewed {
	t.Helper()
	return recoverWith(t, startSteppedDaemon, agent, state, before)
}

// recoverWith seeds a ticket with an open pull request, starts a daemon
// with start and recovers the ticket.
func recoverWith(t *testing.T, start func(t *testing.T, dataDir, config string, env ...string) *daemon, agent, state string, before func(gh *githubtest.Server, pr int)) reviewed {
	t.Helper()
	server := gitServer(t)
	gh := githubtest.New(t)
	gh.SetFile("go.mod", "module x\n")
	n := gh.Seed("Add rate limits", strings.SplitN(conforming, "\n", 2)[1], "alex", "istari:"+state)
	branch := fmt.Sprintf("istari/%d/add-rate-limits", n)
	seed := filepath.Join(server, "seed")
	git(t, seed, "switch", "-q", "-c", branch)
	if err := os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("the feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "feature")
	git(t, seed, "push", "-q", "origin", branch)
	pr := gh.SeedPull(branch, "feature")
	if before != nil {
		before(gh, pr)
	}
	// The journal says where the pull request is: recover watches it again.
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: n}
	body, err := journal.Render(journal.Entry{Kind: journal.Checkpoint, Actor: "agent:implementer-1", At: time.Now().UTC(),
		Checkpoint: &journal.CheckpointEntry{Done: "pull request opened", PR: fmt.Sprintf("https://github.com/%s/pull/%d", githubtest.Repo, pr)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token}).Comment(context.Background(), ref, body); err != nil {
		t.Fatal(err)
	}

	dataDir := newDataDir(t)
	log := filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	exectest.Write(t, filepath.Join(bin, "claude"), fmt.Sprintf(agent, binary, log))
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"GITHUB_SERVER_URL=file://" + server, "PATH=" + bin + string(os.PathListSeparator) + fakeGH(t, gh.Token), "GITHUB_API_URL=" + gh.URL}
	d := start(t, dataDir, config, env...)
	d.mustCLI(t, nil, "recover")
	return reviewed{d: d, gh: gh, ref: ref, pr: pr, log: log}
}

func TestAReviewersFindingIsPublishedAndReturnsTheTicketForRework(t *testing.T) {
	t.Parallel()
	r := newReviewed(t, reviewAgent)
	d, gh, ref, pr, log := r.d, r.gh, r.ref, r.pr, r.log

	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var tk struct{ State string }
		d.mustCLI(t, &tk, "ticket", "show", ref.String())
		if tk.State == "in_progress" {
			break
		}
		if time.Now().After(deadline) {
			out, _ := os.ReadFile(log)
			t.Fatalf("the ticket is %s after a minute, want in_progress (T7)\nagent log:\n%s\ndaemon:\n%s", tk.State, out, d.stderr)
		}
	}
	out, _ := os.ReadFile(log)
	if !strings.Contains(string(out), "token_forbidden") {
		t.Fatalf("the reviewer's own transition was not refused:\n%s", out)
	}
	if !strings.Contains(string(out), "`feature.txt` (outside the plan's zone)") {
		t.Fatalf("the reviewer's brief does not list the changed file (the journal has no plan, so no zone):\n%s", out)
	}
	reviews := gh.Issue(pr).Reviews
	if len(reviews) != 1 || !strings.Contains(reviews[0].Body, "## Spec") || !strings.Contains(reviews[0].Body, "nothing refuses a burst") {
		t.Fatalf("reviews = %+v, want the report published under its axis", reviews)
	}
}

// correctingReviewer submits a report that is no YAML, then a corrected one.
const correctingReviewer = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = reviewer ]; then
  printf 'standards: [unclosed\n' | "$istari" review submit --file - >> "$log" 2>&1
  "$istari" review submit --file - >> "$log" 2>&1 <<'REPORT'
standards: 0 findings
spec: 1 finding, a burst is not refused
findings:
  - {axis: spec, kind: missing, file: feature.txt, line: 1, source: ticket, quote: A burst is refused., text: nothing refuses a burst}
REPORT
  "$istari" complete "reviewed" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAReportCorrectedInTheSameRunCompletesTheReview(t *testing.T) {
	t.Parallel()
	r := newReviewed(t, correctingReviewer)
	reviewers := endedReviews(t, r.d, r.log, 1)
	out, _ := os.ReadFile(r.log)
	if !strings.Contains(string(out), "yaml") {
		t.Fatalf("the first report was not refused:\n%s", out)
	}
	if reviewers[0].Reason != "completed: reviewed" {
		t.Fatalf("the reviewer run ended %q, want it completed: its refused report was corrected\nagent log:\n%s", reviewers[0].Reason, out)
	}
	// A completed task gets no second attempt: the review is not done again.
	if task := showTask(t, r.d, reviewers[0].Task); task.State != "completed" {
		t.Fatalf("the review task is %s, want completed", task.State)
	}
	entries := 0
	for _, c := range r.gh.Issue(r.ref.Number).Comments {
		if strings.Contains(c.Body, "**Review**") {
			entries++
		}
	}
	if entries != 1 || len(r.gh.Issue(r.pr).Reviews) != 1 {
		t.Fatalf("review entries = %d, pull request reviews = %d, want one of each", entries, len(r.gh.Issue(r.pr).Reviews))
	}
}

// silentReviewer completes without a report.
const silentReviewer = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
[ "$ISTARI_ROLE" = reviewer ] && "$istari" complete "looks fine" >> "$log" 2>&1
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestAReviewerCannotCompleteWithoutAReport(t *testing.T) {
	t.Parallel()
	r := newReviewed(t, silentReviewer)
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var runs []struct{ Role, State, Reason string }
		r.d.mustCLI(t, &runs, "run", "list")
		if len(runs) > 0 && runs[0].State == "ended" {
			if !strings.Contains(runs[0].Reason, "without istari complete") {
				t.Fatalf("the run ended %q, want it failed: its complete was refused", runs[0].Reason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reviewer run ended after a minute: %+v\n%s", runs, r.d.stderr)
		}
	}
	out, _ := os.ReadFile(r.log)
	if !strings.Contains(string(out), "istari review submit") {
		t.Fatalf("the refusal does not say what is missing:\n%s", out)
	}
	var tk struct{ State string }
	r.d.mustCLI(t, &tk, "ticket", "show", r.ref.String())
	if tk.State != "in_review" {
		t.Fatalf("the ticket is %s, want in_review: no report, no verdict", tk.State)
	}
}

// earlyReviewAgent plays the planner and the implementer like stageAgent, and
// as the reviewer submits the report given as %[3]s and ends its run
// without istari complete, as a refused submit tells it to.
const earlyReviewAgent = `#!/bin/sh
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
  "$istari" complete "planned" >> "$log" 2>&1
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
  "$istari" complete "implemented" >> "$log" 2>&1
  ;;
reviewer)
  "$istari" review submit --file - >> "$log" 2>&1 <<'REPORT'
%[3]s
REPORT
  ;;
esac
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

// earlyReviewPipeline gives review only implement to wait for, so the review
// task is handed out while the ticket is still in_progress (as before #137).
const earlyReviewPipeline = `[checks]
typecheck = "true"
test = "true"
full = "true"
verify_red = false

[[stage]]
name = "plan"
role = "planner"

[[stage]]
name = "implement"
role = "implementer"
needs = ["plan"]
isolation = "worktree"

[[stage]]
name = "review"
role = "reviewer"
needs = ["implement"]
`

// earlyReview starts a daemon on a ready ticket of a project with
// earlyReviewPipeline, whose reviewer submits report.
func earlyReview(t *testing.T, report string) (d *daemon, gh *githubtest.Server, ticket int, log string) {
	t.Helper()
	return pipelineDaemon(t, earlyReviewPipeline, func(log string) string { return fmt.Sprintf(earlyReviewAgent, binary, log, report) })
}

// pipelineDaemon starts a daemon on a ready ticket of a project with
// pipeline, whose claude is the script agent writes for the agent log.
func pipelineDaemon(t *testing.T, pipeline string, agent func(log string) string) (d *daemon, gh *githubtest.Server, ticket int, log string) {
	t.Helper()
	server := gitServer(t)
	seed := filepath.Join(server, "seed")
	if err := os.MkdirAll(filepath.Join(seed, ".istari"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".istari", "pipeline.toml"), []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "pipeline")
	git(t, seed, "push", "-q", "origin", "main")

	gh = githubtest.New(t)
	gh.SetFile("go.mod", "module x\n")
	gh.SetFile(".istari/pipeline.toml", pipeline) // the stages are read from the repository's main branch
	ticket = gh.Seed("Add rate limits", strings.SplitN(conforming, "\n", 2)[1], "alex")
	dataDir := newDataDir(t)
	log = filepath.Join(t.TempDir(), "agent.log")
	bin := t.TempDir()
	exectest.Write(t, filepath.Join(bin, "claude"), agent(log))
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d = startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server,
		"PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	return d, gh, ticket, log
}

// agentRun is a run of any role as run list shows it.
type agentRun struct{ Role, Task, State, Reason string }

// endedReviews waits until n reviewer runs have ended and returns them.
func endedReviews(t *testing.T, d *daemon, log string, n int) []agentRun {
	t.Helper()
	return endedRuns(t, d, log, "reviewer", n)
}

// endedRuns waits until n runs of role have ended and returns them.
func endedRuns(t *testing.T, d *daemon, log, role string, n int) []agentRun {
	t.Helper()
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var runs, ended []agentRun
		d.mustCLI(t, &runs, "run", "list")
		for _, r := range runs {
			if r.Role == role && r.State == "ended" {
				ended = append(ended, r)
			}
		}
		if len(ended) >= n {
			return ended
		}
		if time.Now().After(deadline) {
			out, _ := os.ReadFile(log)
			t.Fatalf("%d %s runs ended, want %d: %+v\nagent log:\n%s\ndaemon:\n%s", len(ended), role, n, runs, out, d.stderr)
		}
	}
}

type taskView struct {
	State    string
	Attempts int
}

func showTask(t *testing.T, d *daemon, id string) taskView {
	t.Helper()
	var task taskView
	d.mustCLI(t, &task, "task", "show", id)
	return task
}

const validReport = `standards: 0 findings
spec: 0 findings
findings: []`

func TestAReviewRefusedForTheTicketsStateCostsNoAttempt(t *testing.T) {
	t.Parallel()
	d, gh, n, log := earlyReview(t, validReport)
	runs := endedReviews(t, d, log, 2)
	out, _ := os.ReadFile(log)
	if !strings.Contains(string(out), "is in_progress") || !strings.Contains(string(out), "end your run without a result") {
		t.Fatalf("the refusal does not tell the reviewer to end its run without a result:\n%s", out)
	}
	for _, r := range runs {
		if !strings.HasPrefix(r.Reason, "yielded: ") {
			t.Fatalf("a reviewer run ended %q, want it yielded", r.Reason)
		}
	}
	if task := showTask(t, d, runs[0].Task); task.Attempts != 0 || task.State == "failed" {
		t.Fatalf("the review task is %s with %d attempts after refused submits, want no attempt spent", task.State, task.Attempts)
	}
	var notes int
	for _, c := range gh.Issue(n).Comments {
		if strings.Contains(c.Body, "handed out before") {
			notes++
			if !strings.Contains(c.Body, "in_progress") {
				t.Fatalf("the journal entry does not name the ticket's state:\n%s", c.Body)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("%d journal entries say the review was handed out early after %d refused submits, want 1", notes, len(runs))
	}
}

func TestAReviewRefusedForAYAMLErrorStillCostsAnAttempt(t *testing.T) {
	t.Parallel()
	d, _, _, log := earlyReview(t, "findings: [")
	runs := endedReviews(t, d, log, 1)
	if !strings.HasPrefix(runs[0].Reason, "rejected: ") {
		t.Fatalf("the reviewer run ended %q, want it rejected", runs[0].Reason)
	}
	if task := showTask(t, d, runs[0].Task); task.Attempts < 1 {
		t.Fatalf("the review task has %d attempts after a YAML error, want the failed one counted", task.Attempts)
	}
}

// replyingAgent is a reviewer that tries to reply to a finding, then submits
// one; and an implementer that replies to a finding the review does not
// have, then to the one it has.
const replyingAgent = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
case "$ISTARI_ROLE" in
reviewer)
  "$istari" review reply --finding 1 "a reviewer's reply" >> "$log" 2>&1
  "$istari" review submit --file - >> "$log" 2>&1 <<'REPORT'
standards: 0 findings
spec: 1 finding, a burst is not refused
findings:
  - {axis: spec, kind: missing, file: feature.txt, line: 1, source: ticket, quote: A burst is refused., text: nothing refuses a burst}
REPORT
  "$istari" complete "reviewed" >> "$log" 2>&1
  ;;
implementer)
  "$istari" review reply --finding 2 "no such finding" >> "$log" 2>&1
  "$istari" review reply --finding 1 "The proxy in front refuses a burst already." >> "$log" 2>&1
  "$istari" complete "replied" >> "$log" 2>&1
  ;;
esac
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestTheImplementerRepliesToAFindingOfTheLatestReview(t *testing.T) {
	t.Parallel()
	r := newReviewed(t, replyingAgent)
	review := endedReviews(t, r.d, r.log, 1)[0].Task
	endedRuns(t, r.d, r.log, "implementer", 1)
	out, _ := os.ReadFile(r.log)
	if !strings.Contains(string(out), "token_forbidden") {
		t.Fatalf("a reviewer's reply was not refused:\n%s", out)
	}
	if !strings.Contains(string(out), "finding 2 is not in the latest review") {
		t.Fatalf("a reply to finding 2 of a review with one finding was not refused:\n%s", out)
	}
	var replies []string
	for _, c := range r.gh.Issue(r.ref.Number).Comments {
		if strings.Contains(c.Body, "**Reply**") {
			replies = append(replies, c.Body)
		}
	}
	want := "**Reply** to finding 1 of review " + review + " — agent:"
	if len(replies) == 0 || !strings.HasPrefix(replies[0], want) || !strings.Contains(replies[0], "The proxy in front refuses a burst already.") {
		t.Fatalf("reply entries = %q, want one starting %q with the text\nagent log:\n%s", replies, want, out)
	}
}
