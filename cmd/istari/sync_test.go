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

// fakeGH puts a gh on PATH that prints token, as `gh auth token` does, and
// the login alex for `gh api user`.
func fakeGH(t *testing.T, token string) string {
	t.Helper()
	return fakeGHAs(t, token, "alex")
}

func fakeGHAs(t *testing.T, token, login string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = \"auth token\" ] && echo %s\n[ \"$1 $2\" = \"api user\" ] && echo %s\nexit 0\n", token, login)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestIstariSyncMirrorsTheTrackersIssues(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	n := gh.Seed("Add rate limits", "## Context\n…", "bob")
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + fakeGH(t, gh.Token), "GITHUB_API_URL=" + gh.URL}
	d := startDaemonWithConfig(t, dataDir, config, env...)

	var report struct{ Created int }
	d.mustCLI(t, &report, "sync")
	var tk struct{ State, Title, Author string }
	d.mustCLI(t, &tk, "ticket", "show", fmt.Sprintf("gh:%s#%d", githubtest.Repo, n))
	if tk.State != "new" || tk.Title != "Add rate limits" || tk.Author != "bob" {
		t.Fatalf("after istari sync the ticket is %+v (report %+v)", tk, report)
	}
	if labels := gh.Issue(n).Labels; len(labels) != 1 || labels[0] != "istari:new" {
		t.Fatalf("issue labels = %v, want [istari:new]", labels)
	}

	// Only the tracker's line matters here: whether a harness is installed
	// depends on the machine.
	cmd := exec.Command(binary, "--config", config, "doctor")
	cmd.Env = append(os.Environ(), env...)
	out, _ := cmd.CombinedOutput()
	if !trackerOK(string(out)) {
		t.Fatalf("doctor does not report the tracker token as ok:\n%s", out)
	}
}

func trackerOK(doctor string) bool {
	for _, line := range strings.Split(doctor, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "tracker" && fields[1] == "github" {
			return fields[2] == "ok"
		}
	}
	return false
}

func TestIstariSyncWithoutATrackerIsRefused(t *testing.T) {
	t.Parallel()
	d := startDaemon(t, newDataDir(t))
	if _, stderr, code := d.cli(t, "sync"); code != 1 || !strings.Contains(stderr, "sync_disabled") {
		t.Fatalf("istari sync without a tracker: exit %d %q", code, stderr)
	}
}

func TestTicketReadyAdmitsForATrustedMemberOnly(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	body := strings.SplitN(conforming, "\n", 2)[1]
	n := gh.Seed("Add rate limits", body, "carol") // carol may only read: not trusted
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + fakeGH(t, gh.Token), "GITHUB_API_URL=" + gh.URL}
	d := startDaemonWithConfig(t, dataDir, config, env...)
	d.mustCLI(t, nil, "sync")
	ref := fmt.Sprintf("gh:%s#%d", githubtest.Repo, n)
	var tk struct{ State string }
	d.mustCLI(t, &tk, "ticket", "show", ref)
	if tk.State != "new" {
		t.Fatalf("an untrusted author's ticket is %s after sync, want new", tk.State)
	}

	asMallory := []string{"PATH=" + fakeGHAs(t, gh.Token, "mallory")}
	if _, stderr, code := istariEnv(t, t.TempDir(), config, asMallory, "ticket", "ready", ref); code != 1 || !strings.Contains(stderr, "not_trusted") {
		t.Fatalf("ticket ready as mallory: exit %d %q", code, stderr)
	}
	asAlex := []string{"PATH=" + fakeGH(t, gh.Token)}
	if out, stderr, code := istariEnv(t, t.TempDir(), config, asAlex, "ticket", "ready", ref); code != 0 {
		t.Fatalf("ticket ready as alex: exit %d %s %s", code, out, stderr)
	}
	// Admitted by alex; the pipeline may start it at once (T3).
	var events []struct {
		Actor   string
		Payload struct{ Trigger, To string }
	}
	d.mustCLI(t, &events, "tail", "--ticket", ref, "--type", "ticket.transitioned")
	admitted := false
	for _, e := range events {
		admitted = admitted || e.Payload.Trigger == "accept" && e.Actor == "human:alex" && e.Payload.To == "ready"
	}
	if !admitted {
		t.Fatalf("transitions = %+v, want accept by human:alex", events)
	}
	// The table's priority becomes the stored one on admission (spec 4.5).
	if labels := gh.Issue(n).Labels; len(labels) != 2 || labels[0] != "istari:ready" || labels[1] != "priority:P1" {
		t.Fatalf("issue labels = %v, want [istari:ready priority:P1]: the admission is written at once", labels)
	}
}

func TestTicketPrioritySetsTheLabelForAHumanOnly(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	n := gh.Seed("Add rate limits", "", "bob", "bug")
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + fakeGH(t, gh.Token), "GITHUB_API_URL=" + gh.URL}
	d := startDaemonWithConfig(t, dataDir, config, env...)
	d.mustCLI(t, nil, "sync")
	ref := fmt.Sprintf("gh:%s#%d", githubtest.Repo, n)

	if _, stderr, code := d.cli(t, "ticket", "priority", ref, "P0", "--actor", "agent:implementer-1"); code != 1 || !strings.Contains(stderr, "human_only") {
		t.Fatalf("ticket priority as an agent: exit %d %q", code, stderr)
	}
	var tk struct{ Priority string }
	d.mustCLI(t, &tk, "ticket", "priority", ref, "P0", "--actor", "human:alex")
	if tk.Priority != "P0" {
		t.Fatalf("ticket priority answered %+v, want P0", tk)
	}
	labels := gh.Issue(n).Labels
	if !slices.Contains(labels, "priority:P0") || !slices.Contains(labels, "bug") {
		t.Fatalf("issue labels = %v, want priority:P0 written at once, bug kept", labels)
	}
	var events []struct{ Type, Actor string }
	d.mustCLI(t, &events, "tail", "--type", "ticket.prioritized")
	if len(events) != 1 || events[0].Actor != "human:alex" {
		t.Fatalf("ticket.prioritized events = %+v, want one by human:alex", events)
	}
}

func TestARawTicketOfAnOutsiderIsRefinedOnlyAfterATrustedReady(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	n := gh.Seed("Limits", "We should add rate limits.\n", "carol") // carol may only read
	ours := gh.Seed("Quotas", "We should add quotas.\n", "alex")    // alex can write: trusted
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + fakeGH(t, gh.Token), "GITHUB_API_URL=" + gh.URL}
	d := startDaemonWithConfig(t, dataDir, config, env...)
	ref := fmt.Sprintf("gh:%s#%d", githubtest.Repo, n)
	refinesOf := func(ref string) int {
		var tasks []struct{ Ticket, Role string }
		d.mustCLI(t, &tasks, "task", "list")
		c := 0
		for _, task := range tasks {
			if task.Ticket == ref && task.Role == "refiner" {
				c++
			}
		}
		return c
	}
	refines := func() int { return refinesOf(ref) }
	d.mustCLI(t, nil, "sync")
	// The trusted author's raw ticket is refined: then the pipeline has run.
	for deadline := time.Now().Add(10 * time.Second); refinesOf(fmt.Sprintf("gh:%s#%d", githubtest.Repo, ours)) == 0; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a trusted author's raw ticket got no refine task")
		}
	}
	if c := refines(); c != 0 {
		t.Fatalf("an outsider's raw ticket got %d refine tasks", c)
	}
	_, stderr, code := istariEnv(t, t.TempDir(), config, []string{"PATH=" + fakeGH(t, gh.Token)}, "ticket", "ready", ref)
	if code != 0 || !strings.Contains(stderr, "refined first") {
		t.Fatalf("ticket ready as alex on a raw ticket: exit %d %q", code, stderr)
	}
	for deadline := time.Now().Add(10 * time.Second); refines() == 0; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no refine task after a trusted ready")
		}
	}
}
