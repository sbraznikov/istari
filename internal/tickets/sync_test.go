package tickets_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/standard"
)

var ctx = context.Background()

type syncFixture struct {
	gh      *githubtest.Server
	store   *store.SQLite
	tickets *core.Tickets
	sync    *tickets.Syncer
	adapter *github.Provider
}

// newSync syncs acme/web; trusted is its project's trusted list (empty:
// whoever can write).
func newSync(t *testing.T, trusted ...string) *syncFixture {
	t.Helper()
	gh := githubtest.New(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	clock := func() time.Time { return time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC) }
	k := core.NewTickets(s, core.TicketOptions{Clock: clock})
	adapter := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	syncer := tickets.NewSyncer(adapter, k, s, []tickets.SyncRepo{{Repo: githubtest.Repo, Project: "web", Trusted: trusted}}, tickets.SyncOptions{Clock: clock, Standards: standard.ForTracker{Tracker: adapter, DefaultPriority: "P2"}, Needs: standard.Needs})
	return &syncFixture{gh: gh, store: s, tickets: k, sync: syncer, adapter: adapter}
}

func (f *syncFixture) run(t *testing.T) tickets.SyncReport {
	t.Helper()
	report, err := f.sync.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return report
}

func (f *syncFixture) ticket(t *testing.T, n int) core.Ticket {
	t.Helper()
	tk, err := f.tickets.Get(ctx, ghRef(n))
	if err != nil {
		t.Fatalf("ticket #%d: %v", n, err)
	}
	return tk
}

func ghRef(n int) string {
	return tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: n}.String()
}

func (f *syncFixture) events(t *testing.T, filter store.Filter) []store.Event {
	t.Helper()
	events, err := f.store.Read(ctx, "", filter, 0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func istariLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if strings.HasPrefix(l, "istari:") {
			out = append(out, l)
		}
	}
	return out
}

type transition struct{ From, To, Trigger, Actor string }

func (f *syncFixture) transitions(t *testing.T, n int) []transition {
	t.Helper()
	var out []transition
	for _, e := range f.events(t, store.Filter{Ticket: ghRef(n), TypePrefix: "ticket.transitioned"}) {
		var tr transition
		json.Unmarshal(e.Payload, &tr)
		out = append(out, tr)
	}
	return out
}

func TestAnIssueCreatedInTheTrackerAppearsAsNew(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("Add rate limits", "## Context\n…", "bob", "bug")
	report := f.run(t)
	tk := f.ticket(t, n)
	if tk.State != core.TicketNew || tk.Project != "web" || tk.Title != "Add rate limits" || tk.Author != "bob" {
		t.Fatalf("ticket = %+v, want new in project web with its title and author", tk)
	}
	if got := istariLabels(f.gh.Issue(n).Labels); !slices.Equal(got, []string{"istari:new"}) {
		t.Fatalf("istari labels on the issue = %v, want [istari:new]", got)
	}
	if report.Created != 1 {
		t.Fatalf("report = %+v, want one created", report)
	}
	if tr := f.transitions(t, n); len(tr) != 1 || tr[0].Trigger != "create" || tr[0].Actor != "human:bob" {
		t.Fatalf("transitions = %+v, want one create by human:bob", tr)
	}
}

func TestIstarisOwnWritesDoNotCauseASecondTransition(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob")
	f.run(t)
	f.run(t)
	if tr := f.transitions(t, n); len(tr) != 1 {
		t.Fatalf("after two polls: %d transitions, want only the create", len(tr))
	}
	// A transition made in istari (e.g. by the CLI) is written to the tracker…
	if _, err := f.tickets.Fire(ctx, "human:alex", ghRef(n), "cancel"); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	issue := f.gh.Issue(n)
	if issue.State != "closed" || issue.StateReason != "not_planned" || !slices.Equal(istariLabels(issue.Labels), []string{"istari:canceled"}) {
		t.Fatalf("issue after an istari cancel = %s/%s %v", issue.State, issue.StateReason, issue.Labels)
	}
	// …and reading it back is not taken for a human's request.
	f.run(t)
	if tr := f.transitions(t, n); len(tr) != 2 {
		t.Fatalf("transitions = %+v, want create and cancel only", tr)
	}
	if got := f.events(t, store.Filter{TypePrefix: "sync.reverted"}); len(got) != 0 {
		t.Fatalf("istari reverted its own write: %+v", got)
	}
}

func TestALegalLabelChangeByAnAuthorizedUserTransitionsTheTicket(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", conforming, "bob")
	f.run(t)
	f.gh.SetPermission("bob", "write")
	f.gh.HumanSetLabels(n, "bob", "istari:ready")
	report := f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready", tk.State)
	}
	tr := f.transitions(t, n)
	if last := tr[len(tr)-1]; last.Trigger != "accept" || last.Actor != "human:bob" || last.From != "new" || last.To != "ready" {
		t.Fatalf("last transition = %+v, want accept new→ready by human:bob", last)
	}
	if report.Transitioned != 1 {
		t.Fatalf("report = %+v", report)
	}
	if got := istariLabels(f.gh.Issue(n).Labels); !slices.Equal(got, []string{"istari:ready"}) {
		t.Fatalf("labels = %v", got)
	}
}

func TestAnIllegalOrUnauthorizedLabelChangeIsRevertedWithAReason(t *testing.T) {
	for _, tc := range []struct {
		name, who, perm, label, says string
	}{
		{"illegal", "bob", "write", "istari:done", "new"},
		{"unauthorized", "mallory", "read", "istari:ready", "mallory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSync(t)
			n := f.gh.Seed("t", "", "bob")
			f.run(t)
			f.gh.SetPermission(tc.who, tc.perm)
			f.gh.HumanSetLabels(n, tc.who, "bug", tc.label)
			report := f.run(t)
			if tk := f.ticket(t, n); tk.State != core.TicketNew {
				t.Fatalf("state = %s, want it unchanged", tk.State)
			}
			issue := f.gh.Issue(n)
			if got := istariLabels(issue.Labels); !slices.Equal(got, []string{"istari:new"}) {
				t.Fatalf("labels = %v, want the state label restored", issue.Labels)
			}
			if !slices.Contains(issue.Labels, "bug") {
				t.Fatalf("the revert removed a label that is not istari's: %v", issue.Labels)
			}
			if len(issue.Comments) != 1 || !strings.Contains(issue.Comments[0].Body, tc.says) || !strings.Contains(issue.Comments[0].Body, tc.label) {
				t.Fatalf("comments = %+v, want one that explains the revert", issue.Comments)
			}
			if report.Reverted != 1 || len(f.events(t, store.Filter{TypePrefix: "sync.reverted"})) != 1 {
				t.Fatalf("report = %+v, want one revert and its event", report)
			}
			f.run(t) // the revert is istari's own write: nothing more happens
			if got := f.gh.Issue(n).Comments; len(got) != 1 {
				t.Fatalf("a second poll commented again: %+v", got)
			}
		})
	}
}

func TestAChangeMissedByOnePollIsRepairedByTheNext(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", conforming, "carol") // carol may not write, so she is not trusted
	f.run(t)
	f.gh.HumanSetLabels(n, "alex", "istari:ready")
	f.gh.FailNext(1)
	if _, err := f.sync.Sync(ctx); err == nil {
		t.Fatal("a poll that met a server error reported success")
	}
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("after the failed poll: %s", tk.State)
	}
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("after the next poll: %s, want ready", tk.State)
	}
}

func TestAHumanCommentProducesOneEvent(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob")
	f.run(t)
	f.gh.HumanComment(n, "bob", "Use the v2 API.")
	if _, err := f.adapter.Comment(ctx, tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: n}, "istari's own note"); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	f.run(t)
	comments := f.events(t, store.Filter{TypePrefix: "sync.comment"})
	if len(comments) != 1 {
		t.Fatalf("%d comment events, want 1 (istari's own comment is not one)", len(comments))
	}
	var p struct{ Author, Body string }
	json.Unmarshal(comments[0].Payload, &p)
	if p.Author != "bob" || p.Body != "Use the v2 API." || comments[0].Ticket != ghRef(n) || comments[0].Actor != "human:bob" {
		t.Fatalf("comment event = %+v %+v", comments[0], p)
	}
}

func TestClosingAndReopeningAnIssueRequestCancelAndReopen(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", "", "alex")
	f.run(t)
	f.gh.HumanClose(n, "alex", "not_planned")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketCanceled {
		t.Fatalf("after a human closed the issue: %s, want canceled", tk.State)
	}
	f.gh.HumanReopen(n, "alex")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("after a human reopened it: %s, want new", tk.State)
	}
	if got := istariLabels(f.gh.Issue(n).Labels); !slices.Equal(got, []string{"istari:new"}) || f.gh.Issue(n).State != "open" {
		t.Fatalf("issue = %s %v", f.gh.Issue(n).State, got)
	}
}

func TestAnIssueFirstSeenWithAStateLabelIsARequestToo(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", conforming, "alex", "istari:ready") // alex is an admin
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready", tk.State)
	}
	if tr := f.transitions(t, n); len(tr) != 2 || tr[1].Trigger != "accept" {
		t.Fatalf("transitions = %+v, want create then accept", tr)
	}
}

func TestAStateLabelAddedNextToTheOldOneIsSeen(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", conforming, "bob")
	f.run(t)
	f.gh.SetPermission("bob", "write")
	f.gh.HumanAddLabel(n, "bob", "istari:ready") // istari:new stays on the issue
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready", tk.State)
	}
	if got := istariLabels(f.gh.Issue(n).Labels); !slices.Equal(got, []string{"istari:ready"}) {
		t.Fatalf("labels = %v, want exactly istari:ready", got)
	}
}

func TestOnlyTransitionsAHumanMayStartAreApplied(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", conforming, "alex") // alex administers the repository
	f.run(t)
	f.gh.HumanSetLabels(n, "alex", "istari:ready")
	f.run(t)
	// ready → planning is T3 start, which the system initiates (spec 4.1).
	f.gh.HumanSetLabels(n, "alex", "istari:planning")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready: a human cannot start planning", tk.State)
	}
	comments := f.gh.Issue(n).Comments
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "istari makes this transition itself") {
		t.Fatalf("comments = %+v, want one that explains the revert", comments)
	}
}

func TestRemovingTheStateLabelIsRevertedWithAReason(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", "", "alex")
	f.run(t)
	f.gh.HumanSetLabels(n, "alex", "bug")
	f.run(t)
	issue := f.gh.Issue(n)
	if got := istariLabels(issue.Labels); !slices.Equal(got, []string{"istari:new"}) {
		t.Fatalf("labels = %v, want istari:new restored", issue.Labels)
	}
	if len(issue.Comments) != 1 || !strings.Contains(issue.Comments[0].Body, "removing the state label") {
		t.Fatalf("comments = %+v", issue.Comments)
	}
}

func TestCommentsFromBeforeIstariSawTheTicketAreNotEvents(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob")
	f.gh.HumanComment(n, "bob", "an old discussion")
	f.run(t)
	if got := f.events(t, store.Filter{TypePrefix: "sync.comment"}); len(got) != 0 {
		t.Fatalf("%d events for comments older than the ticket in istari", len(got))
	}
	f.gh.HumanComment(n, "bob", "a new answer")
	f.run(t)
	got := f.events(t, store.Filter{TypePrefix: "sync.comment"})
	if len(got) != 1 || !strings.Contains(string(got[0].Payload), "a new answer") {
		t.Fatalf("comment events = %+v, want only the new one", got)
	}
}

func TestATicketMadeInIstariForAClosedIssueDoesNotReopenIt(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", "", "alex")
	f.gh.HumanClose(n, "alex", "completed")
	if _, err := f.tickets.Create(ctx, "human:alex", ghRef(n), "web"); err != nil { // e.g. istari ticket create
		t.Fatal(err)
	}
	f.run(t)
	if issue := f.gh.Issue(n); issue.State != "closed" {
		t.Fatalf("the closed issue was reopened: %+v", issue)
	}
	if tk := f.ticket(t, n); tk.State != core.TicketCanceled {
		t.Fatalf("state = %s, want canceled: the issue is closed", tk.State)
	}
}

func TestAChangeByAnUnknownAuthorSaysSo(t *testing.T) {
	f := newSync(t)
	n := f.gh.Seed("t", "", "alex")
	f.run(t)
	f.gh.HumanSetLabels(n, "", "istari:ready") // e.g. a deleted account: no actor
	f.run(t)
	comments := f.gh.Issue(n).Comments
	if len(comments) != 1 || strings.Contains(comments[0].Body, "@ ") || !strings.Contains(comments[0].Body, "unknown") {
		t.Fatalf("comments = %+v, want one that says the author is unknown", comments)
	}
}

// conforming is a ticket body that meets the default standard for acme/web.
const conforming = "## Context\n\nLimits protect the API.\n\n## What needs to be done\n\n- Add a limiter.\n\n" +
	"## Acceptance criteria\n\n- [ ] A burst is refused.\n\n## Out of scope\n\n- Per-user limits.\n\n" +
	"## Dependencies\n\n- Needs: none\n\n---\n\n| Type | Size | Priority | Repository |\n|---|---|---|---|\n| feature | S | P1 | `acme/web` |\n"
