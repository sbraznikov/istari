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

// slotsRow returns the doctor output line of the runner slots check, or "".
func slotsRow(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "runner slots") {
			return line
		}
	}
	return ""
}

func TestDoctorNamesTheSlotPausedBecauseItsHarnessDidNotStart(t *testing.T) {
	t.Parallel()
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
	s.Close()

	gh := githubtest.New(t)
	for range 5 { // issue #5 is the ticket; closed, they are not synced or refined
		gh.HumanClose(gh.Seed("Add limits", "## Context\n\nLimits.\n", "bob"), "bob", "completed")
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// claude is there when the daemon starts, then its updater replaces it:
	// while it is gone, the harness does not start.
	bin := t.TempDir()
	claude := filepath.Join(bin, "claude")
	exectest.Write(t, claude, "#!/bin/sh\necho 1.0.0\n")
	ghDir, _, _ := strings.Cut(fakeGH(t, gh.Token), string(os.PathListSeparator))
	path := "PATH=" + bin + string(os.PathListSeparator) + ghDir + string(os.PathListSeparator) + "/usr/bin:/bin"
	d := startDaemonWithConfig(t, dataDir, config, "GITHUB_SERVER_URL=file://"+server, path, "GITHUB_API_URL="+gh.URL)
	if err := os.Remove(claude); err != nil {
		t.Fatal(err)
	}
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
	for _, want := range []string{"implementer-1", "the harness did not start", "claude"} {
		if !strings.Contains(row, want) {
			t.Errorf("runner slots row lacks %q: %s", want, row)
		}
	}
	if log := d.stderr.String(); !strings.Contains(log, "runner implementer-1: paused for 1m: the harness did not start: ") {
		t.Errorf("the daemon log does not say the slot paused:\n%s", log)
	}
	var got struct {
		State    string
		Attempts int
	}
	d.mustCLI(t, &got, "task", "show", task.ID)
	if got.State != "ready" || got.Attempts != 0 {
		t.Errorf("task is %s with %d attempts, want ready with 0", got.State, got.Attempts)
	}
}
