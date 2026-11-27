package main_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/exectest"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// limitedClaude is a claude whose account is at its usage limit: every run
// prints Claude Code's limit message, spends nothing and exits 1.
const limitedClaude = `#!/bin/sh
if [ "$1" = "--version" ]; then echo 1.0.0; exit 0; fi
echo '{"type":"assistant","session_id":"s","message":{"id":"m1","content":[{"type":"text","text":"You'"'"'ve hit your 5-hour limit · resets 10pm (Europe/Berlin)"}]}}'
echo '{"type":"result","subtype":"success","is_error":true,"session_id":"s","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0}}'
exit 1
`

func TestDoctorShowsTheUsageLimitPauseAndItsResetAndTheRunCostsNoAttempt(t *testing.T) {
	t.Parallel()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	server := gitServer(t)
	dataDir := newDataDir(t)
	s, err := store.Open(filepath.Join(dataDir, api.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := core.NewTickets(s, core.TicketOptions{})
	if _, err := k.CreateWith(ctx, "human:alex", core.NewTicket{Ref: "gh:acme/web#5", Project: "web", Title: "Add limits"}); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []string{"accept", "start", "plan_accepted"} {
		if _, err := k.Fire(ctx, "system", "gh:acme/web#5", tr); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	gh := githubtest.New(t)
	for range 5 { // issue #5 is the ticket; closed, they are not synced or refined
		gh.HumanClose(gh.Seed("Add limits", "## Context\n\nLimits.\n", "bob"), "bob", "completed")
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	exectest.Write(t, filepath.Join(bin, "claude"), limitedClaude)
	ghDir, _, _ := strings.Cut(fakeGH(t, gh.Token), string(os.PathListSeparator))
	path := "PATH=" + bin + string(os.PathListSeparator) + ghDir + string(os.PathListSeparator) + "/usr/bin:/bin"
	// Stepped, with no pass: only the runner acts on the task made by hand.
	d := startSteppedDaemon(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server, path, "GITHUB_API_URL="+gh.URL)
	var task struct{ ID string }
	d.call(t, "task.create", map[string]any{"actor": "system", "ticket": "gh:acme/web#5", "project": "web", "role": "implementer"}, &task)

	var row string
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		out, stderr, _ := d.cli(t, "doctor")
		if row = slotsRow(out + stderr); strings.Contains(row, "FAIL") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("doctor shows no failing runner slots row:\n%s%s\ndaemon: %s", out, stderr, d.stderr)
		}
	}
	// The reset is the next 10pm in Berlin, shown in the local time of the
	// doctor (the test's own process).
	now := time.Now().In(berlin)
	reset := time.Date(now.Year(), now.Month(), now.Day(), 22, 0, 0, 0, berlin)
	if !reset.After(time.Now()) {
		reset = reset.AddDate(0, 0, 1)
	}
	for _, want := range []string{"implementer-1", "paused until " + reset.Local().Format("15:04:05"), "usage limit", "You've hit your 5-hour limit"} {
		if !strings.Contains(row, want) {
			t.Errorf("runner slots row lacks %q: %s", want, row)
		}
	}
	var got struct {
		State    string
		Attempts int
	}
	d.mustCLI(t, &got, "task", "show", task.ID)
	if got.State != "ready" || got.Attempts != 0 {
		t.Errorf("task is %s with %d attempts, want ready with 0", got.State, got.Attempts)
	}
	if types := d.eventTypes(t, "gh:acme/web#5", "harness.limited"); len(types) != 1 {
		t.Errorf("harness.limited events = %v, want one", types)
	}
}
