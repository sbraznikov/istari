package tickets_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

type prioritized struct{ Priority, Actor string }

func (f *syncFixture) prioritized(t *testing.T, n int) []prioritized {
	t.Helper()
	var out []prioritized
	for _, e := range f.events(t, store.Filter{Ticket: ghRef(n), TypePrefix: "ticket.prioritized"}) {
		var p prioritized
		json.Unmarshal(e.Payload, &p)
		out = append(out, p)
	}
	return out
}

func TestThePriorityLabelOfANewIssueBecomesTheTicketsPriority(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	f.gh.SetPermission("bob", "write")
	n := f.gh.Seed("t", "", "bob", "priority:P1")
	f.run(t)
	if tk := f.ticket(t, n); tk.Priority != "P1" {
		t.Fatalf("priority = %q, want P1", tk.Priority)
	}
	if p := f.prioritized(t, n); len(p) != 1 || p[0] != (prioritized{"P1", "human:bob"}) {
		t.Fatalf("prioritized events = %+v, want one P1 by human:bob", p)
	}
	f.run(t)
	if p := f.prioritized(t, n); len(p) != 1 {
		t.Fatalf("a second sync prioritized again: %+v", p)
	}
}

func TestAPriorityLabelChangedByAMemberChangesThePriority(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob", "bug", "priority:P2")
	f.run(t)
	f.gh.SetPermission("alex", "write")
	f.gh.HumanSetLabels(n, "alex", "bug", "istari:new", "priority:P0")
	f.run(t)
	if tk := f.ticket(t, n); tk.Priority != "P0" {
		t.Fatalf("priority = %q, want P0", tk.Priority)
	}
	if p := f.prioritized(t, n); len(p) != 2 || p[1] != (prioritized{"P0", "human:alex"}) {
		t.Fatalf("prioritized events = %+v, want P2 by the author, then P0 by human:alex", p)
	}
	if got := f.events(t, store.Filter{TypePrefix: "sync.reverted"}); len(got) != 0 {
		t.Fatalf("a member's change was reverted: %+v", got)
	}
}

func TestAPriorityLabelChangedByANonMemberIsRevertedWithAReason(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob", "bug", "priority:P2")
	f.run(t)
	f.gh.SetPermission("mallory", "read")
	f.gh.HumanSetLabels(n, "mallory", "bug", "istari:new", "priority:P0")
	f.run(t)
	if tk := f.ticket(t, n); tk.Priority != "P2" {
		t.Fatalf("priority = %q, want P2 unchanged", tk.Priority)
	}
	issue := f.gh.Issue(n)
	if !slices.Contains(issue.Labels, "priority:P2") || slices.Contains(issue.Labels, "priority:P0") || !slices.Contains(issue.Labels, "bug") {
		t.Fatalf("labels = %v, want priority:P2 restored and bug kept", issue.Labels)
	}
	if len(issue.Comments) != 1 || !strings.Contains(issue.Comments[0].Body, "mallory") || !strings.Contains(issue.Comments[0].Body, "priority:P0") {
		t.Fatalf("comments = %+v, want one that explains the revert", issue.Comments)
	}
	f.run(t)
	if got := f.gh.Issue(n).Comments; len(got) != 1 {
		t.Fatalf("a second poll commented again: %+v", got)
	}
}

func TestAPrioritySetInIstariIsWrittenToTheTrackerOnce(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob", "bug")
	f.run(t)
	if _, err := f.tickets.SetPriority(ctx, "human:alex", ghRef(n), "P1"); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	if labels := f.gh.Issue(n).Labels; !slices.Contains(labels, "priority:P1") || !slices.Contains(labels, "istari:new") {
		t.Fatalf("labels = %v, want priority:P1 next to the state", labels)
	}
	f.run(t)
	if p := f.prioritized(t, n); len(p) != 1 {
		t.Fatalf("reading istari's own label back prioritized again: %+v", p)
	}
	if got := f.events(t, store.Filter{TypePrefix: "sync.reverted"}); len(got) != 0 {
		t.Fatalf("istari reverted its own write: %+v", got)
	}
}

func TestAStateAndAPriorityChangedTogetherAreBothApplied(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob", "priority:P2")
	f.run(t)
	f.gh.SetPermission("alex", "write")
	f.gh.HumanSetLabels(n, "alex", "istari:canceled", "priority:P0")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != "canceled" || tk.Priority != "P0" {
		t.Fatalf("ticket = %s at %q, want canceled at P0", tk.State, tk.Priority)
	}
	if labels := f.gh.Issue(n).Labels; !slices.Contains(labels, "priority:P0") {
		t.Fatalf("labels = %v, want priority:P0 kept", labels)
	}
}

func TestTheSizeLabelIsTheTicketsSize(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob", "size:M")
	f.run(t)
	if tk := f.ticket(t, n); tk.Size != "M" {
		t.Fatalf("size = %q, want M", tk.Size)
	}
	f.gh.HumanSetLabels(n, "bob", "istari:new", "size:L")
	f.run(t)
	if tk := f.ticket(t, n); tk.Size != "L" {
		t.Fatalf("size after relabel = %q, want L", tk.Size)
	}
	f.run(t)
	if n := len(f.events(t, store.Filter{Ticket: ghRef(n), TypePrefix: "ticket.sized"})); n != 2 {
		t.Fatalf("ticket.sized events = %d, want 2", n)
	}
}

func TestAPriorityLabelAddedNextToTheOldOneReplacesIt(t *testing.T) {
	t.Parallel()
	for _, order := range [][]string{{"priority:P2", "priority:P0"}, {"priority:P0", "priority:P2"}} {
		f := newSync(t)
		n := f.gh.Seed("t", "", "bob", "priority:P2")
		f.run(t)
		f.gh.SetPermission("alex", "write")
		f.gh.HumanSetLabels(n, "alex", append([]string{"istari:new"}, order...)...)
		f.run(t)
		if tk := f.ticket(t, n); tk.Priority != "P0" {
			t.Fatalf("labels %v: priority = %q, want P0", order, tk.Priority)
		}
		labels := f.gh.Issue(n).Labels
		if slices.Contains(labels, "priority:P2") || !slices.Contains(labels, "priority:P0") {
			t.Fatalf("labels %v: issue has %v, want only priority:P0", order, labels)
		}
	}
}

func TestARepositorysOwnPriorityLabelsAreLeftAlone(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("t", "", "bob", "priority:high", "priority:P1")
	f.run(t)
	if _, err := f.tickets.Fire(ctx, "human:alex", ghRef(n), "cancel"); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	if labels := f.gh.Issue(n).Labels; !slices.Contains(labels, "priority:high") || !slices.Contains(labels, "priority:P1") {
		t.Fatalf("labels = %v, want priority:high kept next to priority:P1", labels)
	}
}

// notes returns the texts of the ticket's journal notes, read from its
// comments.
func (f *syncFixture) notes(t *testing.T, n int) []string {
	t.Helper()
	r, err := tickets.ParseRef(ghRef(n))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := journal.New(f.adapter, f.store, nil).Read(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Kind == journal.Note {
			out = append(out, e.Text)
		}
	}
	return out
}

func withTablePriority(p string) string { return strings.Replace(conforming, "| P1 |", "| "+p+" |", 1) }

func TestAPriorityLabelOutranksTheTableAndTheDifferenceIsNotedOnce(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", withTablePriority("P3"), "alex", "priority:P1")
	f.run(t)
	f.run(t)
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady || tk.Priority != "P1" {
		t.Fatalf("ticket = %s %q, want ready and still P1", tk.State, tk.Priority)
	}
	notes := f.notes(t, n)
	if len(notes) != 1 || !strings.Contains(notes[0], "P1") || !strings.Contains(notes[0], "P3") {
		t.Fatalf("notes = %q, want one naming P1 and P3", notes)
	}
}

func TestAfterAdmissionTheLabelNotTheTableChangesThePriority(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", withTablePriority("P0"), "alex")
	f.run(t)
	f.gh.HumanEditBody(n, "alex", withTablePriority("P3"))
	f.run(t)
	f.run(t)
	if tk := f.ticket(t, n); tk.Priority != "P0" {
		t.Fatalf("priority = %q after the table changed, want P0", tk.Priority)
	}
	if notes := f.notes(t, n); len(notes) != 1 || !strings.Contains(notes[0], "P0") || !strings.Contains(notes[0], "P3") {
		t.Fatalf("notes = %q, want one naming P0 and P3", notes)
	}
	f.gh.HumanSetLabels(n, "alex", "istari:ready", "priority:P2")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady || tk.Priority != "P2" {
		t.Fatalf("ticket = %s %q after the label changed, want ready and P2", tk.State, tk.Priority)
	}
}

func TestTheNeedsInATicketsTextAreItsNeeds(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("t", "## Dependencies\n\n- Needs: #2, acme/api#7\n", "bob")
	f.run(t)
	if got := f.ticket(t, n).Needs; strings.Join(got, " ") != "gh:acme/api#7 gh:acme/web#2" {
		t.Fatalf("needs = %v", got)
	}
	f.gh.HumanEditBody(n, "bob", "## Dependencies\n\n- Needs: none\n")
	f.run(t)
	if got := f.ticket(t, n).Needs; len(got) != 0 {
		t.Fatalf("after the edit needs = %v, want none", got)
	}
}
