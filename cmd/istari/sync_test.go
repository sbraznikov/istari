package main_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/exectest"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/journal"
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
	exectest.Write(t, filepath.Join(dir, "gh"), script)
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
	d := startDaemon(t, newDataDir(t)) //nolint:forbidigo // no project and no due timer: no loop pass decides the outcome
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

// humanMerge is a ticket, in awaiting_approval unless made otherwise, whose
// pull request istari watches, as istari recover leaves it, served by a fake
// GitHub. Its daemon is stepped: each test names the passes it needs.
type humanMerge struct {
	gh        *githubtest.Server
	d         *daemon
	dataDir   string
	config    string
	env       []string
	issue, pr int
	ref       string
}

func newHumanMerge(t *testing.T) *humanMerge {
	t.Helper()
	return recoveredWithPullRequest(t, "awaiting_approval")
}

// recoveredWithPullRequest is a ticket in state whose pull request istari
// watches, as istari recover leaves it.
func recoveredWithPullRequest(t *testing.T, state string) *humanMerge {
	t.Helper()
	h := &humanMerge{gh: githubtest.New(t), dataDir: newDataDir(t)}
	h.issue = h.gh.Seed("Add limits", "## Context\n\nLimits.\n", "alex", "istari:"+state)
	h.pr = h.gh.SeedPull(fmt.Sprintf("istari/%d/limits", h.issue))
	h.ref = fmt.Sprintf("gh:%s#%d", githubtest.Repo, h.issue)
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: h.issue}
	body, err := journal.Render(journal.Entry{Kind: journal.Checkpoint, Actor: "agent:implementer-1", At: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC),
		Checkpoint: &journal.CheckpointEntry{Done: "pull request opened", PR: fmt.Sprintf("https://github.com/%s/pull/%d", githubtest.Repo, h.pr)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := github.New(github.Options{BaseURL: h.gh.URL, Token: h.gh.Token}).Comment(context.Background(), ref, body); err != nil {
		t.Fatal(err)
	}
	h.config = filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", h.dataDir, githubtest.Repo)
	if err := os.WriteFile(h.config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h.env = []string{"PATH=" + fakeGH(t, h.gh.Token), "GITHUB_API_URL=" + h.gh.URL}
	h.d = startSteppedDaemon(t, h.dataDir, h.config, h.env...)
	h.d.mustCLI(t, nil, "recover")
	h.d.step(t, "pipeline") // the tasks of the recovered state
	if st := h.state(t); st != state {
		t.Fatalf("the recovered ticket is %s, want %s", st, state)
	}
	return h
}

func (h *humanMerge) state(t *testing.T) string {
	t.Helper()
	var tk struct{ State string }
	h.d.mustCLI(t, &tk, "ticket", "show", h.ref)
	return tk.State
}

// evidence is what a failed check prints: the ticket's events and the
// daemon's stderr.
func (h *humanMerge) evidence(t *testing.T) string {
	t.Helper()
	events, stderr, _ := h.d.cli(t, "tail", "--ticket", h.ref) // a failed tail shows its stderr in place of the events
	return fmt.Sprintf("events:\n%s%s\ndaemon stderr:\n%s", events, stderr, h.d.stderr)
}

// finished asserts the ticket ended in done the way istari's own merge ends
// it: the issue closed as completed with istari:done, nothing undone.
func (h *humanMerge) finished(t *testing.T) {
	t.Helper()
	if st := h.state(t); st != "done" {
		t.Fatalf("the ticket is %s, want done\n%s", st, h.d.stderr)
	}
	i := h.gh.Issue(h.issue)
	if i.State != "closed" || i.StateReason != "completed" || !slices.Equal(i.Labels, []string{"istari:done"}) {
		t.Fatalf("the issue is %s/%s with %v, want closed/completed with [istari:done]", i.State, i.StateReason, i.Labels)
	}
	if reverted := h.d.eventTypes(t, h.ref, "sync.reverted"); len(reverted) > 0 {
		t.Fatalf("sync undid a change: %v", reverted)
	}
}

func TestAPullRequestAHumanMergesFinishesItsTicketWhenTheWatchSeesItFirst(t *testing.T) {
	t.Parallel()
	h := newHumanMerge(t)
	h.gh.HumanMerge(h.pr, "alex") // GitHub closes the issue after the watch looked
	h.d.step(t, "watch")
	h.d.step(t, "pipeline")
	if st := h.state(t); st != "done" {
		t.Fatalf("after a watch and a pipeline pass the ticket of a merged pull request is %s, want done\n%s", st, h.d.stderr)
	}
	h.gh.HumanClose(h.issue, "alex", "completed") // Closes #n
	h.d.mustCLI(t, nil, "sync")
	h.finished(t)
}

func TestAHumanClosingTheIssueOfATicketWithAnOpenPullRequestCancelsIt(t *testing.T) {
	t.Parallel()
	h := newHumanMerge(t)
	h.gh.HumanClose(h.issue, "alex", "not_planned")
	h.d.mustCLI(t, nil, "sync")
	if st := h.state(t); st != "canceled" {
		t.Fatalf("the ticket of a closed issue is %s, want canceled (T13)", st)
	}
}

func TestAPullRequestClosedWithoutAMergeLeavesItsTicketAlone(t *testing.T) {
	t.Parallel()
	h := newHumanMerge(t)
	h.gh.HumanClose(h.pr, "alex", "")
	h.d.step(t, "watch")
	if closed := h.d.eventTypes(t, h.ref, "vcs.closed"); len(closed) != 1 {
		t.Fatalf("the watch pass recorded %v, want one vcs.closed\n%s", closed, h.evidence(t))
	}
	h.d.step(t, "pipeline")
	h.d.step(t, "sync")
	h.d.step(t, "pipeline")
	if st := h.state(t); st != "awaiting_approval" {
		t.Fatalf("the ticket of a pull request closed without a merge is %s, want awaiting_approval\n%s", st, h.evidence(t))
	}
}

func TestATicketAHumansMergeCanceledIsRepairedToDoneOnTheNextSync(t *testing.T) {
	t.Parallel()
	h := newHumanMerge(t)
	h.gh.HumanClose(h.issue, "alex", "not_planned")
	h.d.mustCLI(t, nil, "sync")
	if st := h.state(t); st != "canceled" {
		t.Fatalf("the ticket is %s, want canceled before the merge", st)
	}
	before := len(h.d.eventTypes(t, h.ref, "ticket.transitioned"))
	h.gh.HumanMerge(h.pr, "alex")
	h.d.step(t, "watch")
	if closed := h.d.eventTypes(t, h.ref, "vcs.closed"); len(closed) != 1 {
		t.Fatalf("the watch pass recorded %v, want one vcs.closed for the merge\n%s", closed, h.d.stderr)
	}
	h.d.mustCLI(t, nil, "sync")
	h.finished(t)
	if after := len(h.d.eventTypes(t, h.ref, "ticket.transitioned")); after != before+1 {
		t.Fatalf("the repair logged %d transitions, want 1", after-before)
	}
}

func TestAPullRequestAHumanMergesFinishesItsTicketWhenSyncSeesTheClosedIssueFirst(t *testing.T) {
	t.Parallel()
	h := newHumanMerge(t)
	h.gh.HumanMerge(h.pr, "alex")
	h.gh.HumanClose(h.issue, "alex", "completed") // Closes #n, before the watch polls again
	h.d.mustCLI(t, nil, "sync")
	h.finished(t)
}

// neededTicket is a paused ready ticket and a ready one that needs it, held
// on waiting_dependency, served by a fake GitHub. Its daemon is stepped, and
// neither ticket is started.
type neededTicket struct {
	gh              *githubtest.Server
	d               *daemon
	need            int
	needRef, depRef string
}

func newNeededTicket(t *testing.T) *neededTicket {
	t.Helper()
	n := &neededTicket{gh: githubtest.New(t)}
	body := strings.SplitN(conforming, "\n", 2)[1]
	n.need = n.gh.Seed("Add limits", body, "alex")
	dependent := n.gh.Seed("Use limits", strings.Replace(body, "- Needs: none", fmt.Sprintf("- Needs: #%d", n.need), 1), "alex")
	n.needRef = fmt.Sprintf("gh:%s#%d", githubtest.Repo, n.need)
	n.depRef = fmt.Sprintf("gh:%s#%d", githubtest.Repo, dependent)
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	n.d = startSteppedDaemon(t, dataDir, config, "PATH="+fakeGH(t, n.gh.Token), "GITHUB_API_URL="+n.gh.URL)
	n.d.mustCLI(t, nil, "sync")
	n.d.mustCLI(t, nil, "ticket", "pause", n.needRef) // stays ready: the pipeline does not start it
	n.d.step(t, "pipeline")
	if st, hold := n.show(t, n.needRef); st != "ready" || hold != "paused" {
		t.Fatalf("the needed ticket is %s, hold %q; want ready, paused", st, hold)
	}
	if st, hold := n.show(t, n.depRef); st != "ready" || hold != "waiting_dependency" {
		t.Fatalf("the dependent ticket is %s, hold %q; want ready on waiting_dependency", st, hold)
	}
	return n
}

func (n *neededTicket) show(t *testing.T, ref string) (state, hold string) {
	t.Helper()
	var tk struct{ State, Hold string }
	n.d.mustCLI(t, &tk, "ticket", "show", ref)
	return tk.State, tk.Hold
}

func TestAHumanClosingAnIssueAsCompletedFinishesItsTicket(t *testing.T) {
	t.Parallel()
	n := newNeededTicket(t)
	n.gh.HumanClose(n.need, "alex", "completed")
	n.d.mustCLI(t, nil, "sync")
	if st, _ := n.show(t, n.needRef); st != "done" {
		t.Fatalf("the ticket of an issue a human closed as completed is %s, want done\n%s", st, n.d.stderr)
	}
	if i := n.gh.Issue(n.need); i.StateReason != "completed" || !slices.Contains(i.Labels, "istari:done") {
		t.Fatalf("the issue is %s with %v, want completed with istari:done", i.StateReason, i.Labels)
	}
	n.d.step(t, "pipeline")
	// Its need done, the pipeline may start it at once (T3).
	if st, hold := n.show(t, n.depRef); st != "ready" && st != "planning" || hold != "" {
		t.Fatalf("the ticket that needs it is %s, hold %q; want ready or planning without a hold", st, hold)
	}
}
