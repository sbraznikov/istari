package main_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	server := gitServer(t)
	gh := githubtest.New(t)
	gh.SetFile("go.mod", "module x\n")
	n := gh.Seed("Add rate limits", strings.SplitN(conforming, "\n", 2)[1], "alex", "istari:in_review")
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
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(agent, binary, log)), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server,
		"PATH="+bin+string(os.PathListSeparator)+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)
	d.mustCLI(t, nil, "recover")
	return reviewed{d: d, gh: gh, ref: ref, pr: pr, log: log}
}

func TestAReviewersFindingIsPublishedAndReturnsTheTicketForRework(t *testing.T) {
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
	r := newReviewed(t, correctingReviewer)
	var reviewers []struct{ Role, State, Reason string }
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var runs []struct{ Role, State, Reason string }
		r.d.mustCLI(t, &runs, "run", "list")
		reviewers = reviewers[:0]
		for _, run := range runs {
			if run.Role == "reviewer" {
				reviewers = append(reviewers, run)
			}
		}
		if len(reviewers) > 0 && reviewers[0].State == "ended" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reviewer run ended after a minute: %+v\n%s", runs, r.d.stderr)
		}
	}
	out, _ := os.ReadFile(r.log)
	if !strings.Contains(string(out), "yaml") {
		t.Fatalf("the first report was not refused:\n%s", out)
	}
	if reviewers[0].Reason != "completed: reviewed" {
		t.Fatalf("the reviewer run ended %q, want it completed: its refused report was corrected\nagent log:\n%s", reviewers[0].Reason, out)
	}
	time.Sleep(time.Second) // a failed task would start a second reviewer run
	var runs []struct{ Role string }
	r.d.mustCLI(t, &runs, "run", "list")
	n := 0
	for _, run := range runs {
		if run.Role == "reviewer" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("reviewer runs = %d, want one: the corrected review is not done again", n)
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
