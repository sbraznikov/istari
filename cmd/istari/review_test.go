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

func TestAReviewersFindingIsPublishedAndReturnsTheTicketForRework(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fmt.Sprintf(reviewAgent, binary, log)), 0o755); err != nil {
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
