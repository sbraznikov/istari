package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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

func TestRecoverRebuildsTheLostDatabaseFromTheTracker(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	held := gh.Seed("Add a cache", "## Context\n\nA cache.\n", "alex", "istari:in_progress", "istari:hold:needs_input", "priority:P1")
	working := gh.Seed("Add limits", "## Context\n\nLimits.\n", "alex", "istari:in_progress")
	// What istari journaled before its database was lost.
	adapter := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: working}
	at := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	for _, e := range []journal.Entry{
		{Kind: journal.Plan, Actor: "agent:planner-1", At: at, Plan: &journal.PlanEntry{Summary: "Limits per key"}},
		{Kind: journal.Checkpoint, Actor: "agent:implementer-1", At: at.Add(time.Minute), Checkpoint: &journal.CheckpointEntry{Done: "pull request #42 opened", PR: "https://github.com/acme/web/pull/42"}},
	} {
		body, err := journal.Render(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Comment(context.Background(), ref, body); err != nil {
			t.Fatal(err)
		}
	}

	dataDir := newDataDir(t) // a new database
	config := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithConfig(t, dataDir, config, "PATH="+fakeGH(t, gh.Token), "GITHUB_API_URL="+gh.URL)

	var synced struct{ Unknown int }
	d.mustCLI(t, &synced, "sync") // the sync leaves lost tickets alone, and says so
	if synced.Unknown != 2 {
		t.Fatalf("sync report = %+v, want 2 unknown", synced)
	}
	if out, _, _ := d.cli(t, "sync"); !strings.Contains(out, "UNKNOWN") || !strings.Contains(strings.Fields(strings.Split(out, "\n")[1])[5], "2") {
		t.Fatalf("istari sync does not show the unknown tickets:\n%s", out)
	}
	if _, stderr, code := d.cli(t, "ticket", "show", ref.String()); code == 0 {
		t.Fatalf("sync took a lost ticket as new: %s", stderr)
	}
	var report struct{ Recovered int }
	d.mustCLI(t, &report, "recover")
	if report.Recovered != 2 {
		t.Fatalf("recover report = %+v, want 2 recovered", report)
	}
	var tk struct{ State, Hold, Priority string }
	d.mustCLI(t, &tk, "ticket", "show", fmt.Sprintf("gh:%s#%d", githubtest.Repo, held))
	if tk.State != "in_progress" || tk.Hold != "needs_input" || tk.Priority != "P1" {
		t.Fatalf("the held ticket = %+v", tk)
	}
	journaled := d.eventTypes(t, ref.String(), "journal.")
	if len(journaled) != 2 || journaled[0] != "journal.plan" || journaled[1] != "journal.checkpoint" {
		t.Fatalf("the journal in istari's log = %v", journaled)
	}
	if tracked := d.eventTypes(t, ref.String(), "vcs.tracked"); len(tracked) != 1 {
		t.Fatalf("the pull request of the journal is not watched again: %v", tracked)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var tasks []struct{ Ticket, Role, State string }
		d.mustCLI(t, &tasks, "task", "list")
		found := false
		for _, task := range tasks {
			found = found || (task.Ticket == ref.String() && task.Role == "implementer" && task.State == "ready")
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the recovered ticket in progress has no ready implementer task: %+v\n%s", tasks, d.stderr)
		}
	}
	if labels := gh.Issue(held).Labels; len(labels) != 3 {
		t.Fatalf("recover changed the tracker's labels: %v", labels)
	}
}

func TestTicketResumeTakesBackATaskWhoseRunIsGone(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	ref := "gh:acme/web#1"
	createTicket(t, d, ref)
	d.createTask(t, "t_1", "implementer") // a task of #1
	var l lease
	d.mustCLI(t, &l, "task", "claim", "t_1", "--actor", "agent:implementer-9") // a run on a machine that died
	d.mustCLI(t, nil, "ticket", "resume", ref)
	var task struct{ State, Owner string }
	d.mustCLI(t, &task, "task", "show", "t_1")
	if task.State != "ready" || task.Owner != "" {
		t.Fatalf("after resume the task is %+v, want ready", task)
	}
	if _, stderr, code := d.cli(t, "ticket", "resume", ref, "--actor", "agent:implementer-9"); code != 1 || !strings.Contains(stderr, "human_only") {
		t.Fatalf("an agent resumed a ticket: exit %d %q", code, stderr)
	}
}

// eventTypes returns the types of a ticket's events whose type starts with prefix.
func (d *daemon) eventTypes(t *testing.T, ticket, prefix string) []string {
	t.Helper()
	r, err := d.client.Get("http://istari/v1/events?ticket=" + url.QueryEscape(ticket) + "&type=" + url.QueryEscape(prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var events []struct{ Type string }
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}
