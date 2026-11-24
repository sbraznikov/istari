package tickets_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/store"
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
