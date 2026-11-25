package tickets_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/standard"
)

func ref(n int) string { return ghRef(n) }

func TestAConformingTicketByATrustedAuthorBecomesReadyWithoutAHuman(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", conforming, "alex") // alex can write, so is trusted
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready", tk.State)
	}
	tr := f.transitions(t, n)
	if len(tr) != 2 || tr[0].Actor != "human:alex" || tr[1].Trigger != "accept" || tr[1].Actor != "system" {
		t.Fatalf("transitions = %+v, want create by human:alex then accept by the system", tr)
	}
	if tr[1].Rule != "trusted_author" || tr[1].Parent != "" {
		t.Fatalf("T2 = %+v, want the rule trusted_author and no parent", tr[1])
	}
	if got := istariLabels(f.gh.Issue(n).Labels); !slices.Equal(got, []string{"istari:ready"}) {
		t.Fatalf("labels = %v", got)
	}
}

func TestATicketAnAgentCreatesIsCreatedByTheAgentAndAdmittedAsAnAgents(t *testing.T) {
	t.Parallel()
	f := newSync(t, "alex")
	parent := f.gh.Seed("Parent", conforming, "alex")
	f.gh.SetPermission("alex", "write")
	f.run(t)
	child := f.gh.Seed("Found while working", conforming, "alex") // the token owner, as GitHub shows it
	refs := []tickets.Ref{{Provider: "gh", Repo: githubtest.Repo, Number: child}}
	if err := f.sync.Created(ctx, "agent:implementer-2", refs, ref(parent)); err != nil {
		t.Fatalf("Created: %v", err)
	}
	tr := f.transitions(t, child)
	if len(tr) != 2 || tr[0].Trigger != "create" || tr[0].Actor != "agent:implementer-2" {
		t.Fatalf("transitions = %+v, want create by agent:implementer-2", tr)
	}
	if tr[1].Trigger != "accept" || tr[1].Rule != "agent" || tr[1].Parent != ref(parent) {
		t.Fatalf("T2 = %+v, want accept by the rule agent with the parent %s", tr[1], ref(parent))
	}
}

func TestATicketThatMissesTheStandardIsNotAdmitted(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", strings.Replace(conforming, "## Out of scope", "## Notes", 1), "alex")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("state = %s, want new: the ticket misses a section", tk.State)
	}
	if err := f.sync.Ready(ctx, "alex", ref(n)); err != nil {
		t.Fatalf("Ready = %v, want the ticket refined first (spec 4.5)", err)
	}
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("state = %s after Ready, want new until it meets the standard", tk.State)
	}
}

func TestAnUntrustedAuthorsTicketWaitsForATrustedMember(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", conforming, "carol") // carol may only read
	f.run(t)
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("state = %s, want new until a trusted member admits it", tk.State)
	}
	err := f.sync.Ready(ctx, "mallory", ref(n))
	var e *core.Error
	if !errors.As(err, &e) || e.Code != tickets.CodeNotTrusted {
		t.Fatalf("Ready by a non-member = %v, want not_trusted", err)
	}
	if err := f.sync.Ready(ctx, "alex", ref(n)); err != nil {
		t.Fatalf("Ready by alex: %v", err)
	}
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready", tk.State)
	}
	if got := istariLabels(f.gh.Issue(n).Labels); !slices.Equal(got, []string{"istari:ready"}) {
		t.Fatalf("labels after ticket ready = %v", got)
	}
	if tr := f.transitions(t, n); tr[len(tr)-1].Actor != "human:alex" || tr[len(tr)-1].Rule != "human_admission" {
		t.Fatalf("transitions = %+v, want the accept by human:alex by the rule human_admission", tr)
	}
}

func TestAnExplicitTrustedListReplacesWriteAccess(t *testing.T) {
	t.Parallel()
	f := newSync(t, "carol")
	byAlex := f.gh.Seed("a", conforming, "alex") // can write, but not on the list
	byCarol := f.gh.Seed("c", conforming, "carol")
	f.run(t)
	if f.ticket(t, byAlex).State != core.TicketNew || f.ticket(t, byCarol).State != core.TicketReady {
		t.Fatalf("states = %s / %s, want only carol's admitted", f.ticket(t, byAlex).State, f.ticket(t, byCarol).State)
	}
	// alex's istari:ready label is refused too: admission is the list's.
	f.gh.HumanSetLabels(byAlex, "alex", "istari:ready")
	f.run(t)
	if f.ticket(t, byAlex).State != core.TicketNew {
		t.Fatal("a label by someone off the trusted list admitted the ticket")
	}
}

func TestEditingARequiredSectionInReadyReturnsTheTicketToNew(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", conforming, "carol")
	f.run(t)
	if err := f.sync.Ready(ctx, "alex", ref(n)); err != nil {
		t.Fatal(err)
	}
	f.gh.HumanEditBody(n, "carol", strings.Replace(conforming, "A burst is refused.", "A burst is refused with 429.", 1))
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("state = %s, want new after a required section changed", tk.State)
	}
	tr := f.transitions(t, n)
	if last := tr[len(tr)-1]; last.Trigger != "edited" || last.From != "ready" {
		t.Fatalf("last transition = %+v, want edited from ready", last)
	}
}

func TestEditsOutsideTheRequiredSectionsLeaveTheTicketReady(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", conforming, "carol")
	f.run(t)
	if err := f.sync.Ready(ctx, "alex", ref(n)); err != nil {
		t.Fatal(err)
	}
	f.gh.HumanAddLabel(n, "carol", "bug")
	f.gh.HumanEditBody(n, "carol", conforming+"\n## Notes\n\nSee the incident report.\n")
	f.gh.HumanEditBody(n, "carol", strings.Replace(conforming+"\n## Notes\n\nSee the incident report.\n", "Limits protect", "Limits  protect", 1)) // whitespace only
	f.run(t)
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready", tk.State)
	}
}

func TestTheRepositorysOwnStandardIsReadFromTheTracker(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	f.gh.SetFile(".istari/ticket-standard.toml", "sections = [\"Context\", \"Risks\"]\n")
	n := f.gh.Seed("Add limits", conforming, "alex")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("state = %s, want new: this repository requires a Risks section", tk.State)
	}
	_ = githubtest.Repo
}

// labelAdmission switches the fixture's project to admission = "label".
func labelAdmission(f *syncFixture, trusted ...string) {
	f.sync = tickets.NewSyncer(f.adapter, f.tickets, f.store,
		[]tickets.SyncRepo{{Repo: githubtest.Repo, Project: "web", Trusted: trusted, LabelOnly: true}},
		tickets.SyncOptions{Standards: standard.ForTracker{Tracker: f.adapter, DefaultPriority: "P2"}, Needs: standard.Needs, Parent: standard.Parent})
}

func TestInLabelModeATrustedAuthorsTicketWaitsInNew(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	labelAdmission(f)
	n := f.gh.Seed("Add limits", conforming, "alex") // alex can write, so is trusted
	f.run(t)
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("state = %s, want new until a trusted member admits it", tk.State)
	}
	if ok, err := f.sync.Refinable(ctx, ref(n)); err != nil || ok {
		t.Fatalf("Refinable = %v, %v; want false: no refiner run before admission", ok, err)
	}
}

func TestInLabelModeATrustedMembersReadyLabelAdmits(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	labelAdmission(f)
	n := f.gh.Seed("Add limits", conforming, "alex")
	f.run(t)
	f.gh.HumanSetLabels(n, "alex", "istari:ready")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready after alex's label", tk.State)
	}
	if tr := f.transitions(t, n); tr[len(tr)-1].Trigger != "accept" || tr[len(tr)-1].Actor != "human:alex" || tr[len(tr)-1].Rule != "human_admission" {
		t.Fatalf("transitions = %+v, want the accept by human:alex by the rule human_admission", tr)
	}
}

func TestInLabelModeAnUntrustedReadyLabelIsReverted(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	labelAdmission(f)
	n := f.gh.Seed("Add limits", conforming, "alex")
	f.run(t)
	f.gh.HumanSetLabels(n, "carol", "istari:ready") // carol may only read
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("state = %s, want new", tk.State)
	}
	if got := istariLabels(f.gh.Issue(n).Labels); !slices.Equal(got, []string{"istari:new"}) {
		t.Fatalf("labels = %v, want istari:new restored", got)
	}
	comments := f.gh.Issue(n).Comments
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "not a trusted member") {
		t.Fatalf("comments = %+v, want one saying carol is not trusted", comments)
	}
}

func TestInLabelModeTicketReadyAdmitsAndSponsorsATicketBelowTheStandard(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	labelAdmission(f)
	short := strings.Replace(conforming, "## Out of scope", "## Notes", 1)
	n := f.gh.Seed("Add limits", conforming, "alex")
	m := f.gh.Seed("Add quotas", short, "alex")
	f.run(t)
	for _, r := range []int{n, m} {
		if err := f.sync.Ready(ctx, "alex", ref(r)); err != nil {
			t.Fatalf("Ready %d: %v", r, err)
		}
	}
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready after istari ticket ready", tk.State)
	}
	if tk := f.ticket(t, m); tk.State != core.TicketNew {
		t.Fatalf("state = %s, want new until it meets the standard", tk.State)
	}
	if ok, err := f.sync.Refinable(ctx, ref(m)); err != nil || !ok {
		t.Fatalf("Refinable = %v, %v; want true: a trusted member asked to admit it", ok, err)
	}
	f.gh.HumanEditBody(m, "alex", conforming)
	f.run(t)
	if tk := f.ticket(t, m); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready once it meets the standard", tk.State)
	}
}

func TestInLabelModeATicketAnAgentCreatesInheritsTheAdmission(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	labelAdmission(f)
	parent := f.gh.Seed("Parent", conforming, "alex")
	f.run(t)
	if err := f.sync.Ready(ctx, "alex", ref(parent)); err != nil {
		t.Fatal(err)
	}
	child := f.gh.Seed("Found while working", conforming, "alex")
	short := f.gh.Seed("Half B", strings.Replace(conforming, "## Out of scope", "## Notes", 1), "alex")
	refs := []tickets.Ref{{Provider: "gh", Repo: githubtest.Repo, Number: child}, {Provider: "gh", Repo: githubtest.Repo, Number: short}}
	if err := f.sync.Created(ctx, "agent:implementer-1", refs, ref(parent)); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if tk := f.ticket(t, child); tk.State != core.TicketReady {
		t.Fatalf("the agent's ticket = %s, want ready without the label", tk.State)
	}
	if ok, err := f.sync.Refinable(ctx, ref(short)); err != nil || !ok {
		t.Fatalf("Refinable = %v, %v; want true: it inherits the admission", ok, err)
	}
	f.gh.HumanEditBody(short, "alex", conforming) // as the refiner would
	f.run(t)
	if tk := f.ticket(t, short); tk.State != core.TicketReady {
		t.Fatalf("the refined child = %s, want ready once it meets the standard", tk.State)
	}
}

func TestATicketAlreadyReadyStaysReadyWhenTheProjectSwitchesToLabel(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", conforming, "alex")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s, want ready without the key", tk.State)
	}
	labelAdmission(f)
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady {
		t.Fatalf("state = %s after the switch, want ready", tk.State)
	}
}

func TestAReadyLabelOnATicketBelowTheStandardIsReverted(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("t", strings.Replace(conforming, "## Out of scope", "## Notes", 1), "carol")
	f.run(t)
	f.gh.HumanSetLabels(n, "alex", "istari:ready") // alex is trusted, the ticket is not ready
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketNew {
		t.Fatalf("state = %s, want new", tk.State)
	}
	comments := f.gh.Issue(n).Comments
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "Out of scope") {
		t.Fatalf("comments = %+v, want one naming the missing section", comments)
	}
}

func TestTheTablesPriorityBecomesThePriorityOnAdmission(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", strings.Replace(conforming, "| P1 |", "| P0 |", 1), "alex")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady || tk.Priority != "P0" {
		t.Fatalf("ticket = %s %q, want ready and P0 from its table", tk.State, tk.Priority)
	}
	if p := f.prioritized(t, n); len(p) != 1 || p[0] != (prioritized{"P0", "human:alex"}) {
		t.Fatalf("prioritized events = %+v, want one P0 by the author human:alex", p)
	}
	if !slices.Contains(f.gh.Issue(n).Labels, "priority:P0") {
		t.Fatalf("labels = %v, want priority:P0", f.gh.Issue(n).Labels)
	}
}

func TestATicketAdmittedByTheReadyLabelGetsItsTablesPriority(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", conforming, "carol") // carol is not trusted
	f.run(t)
	f.gh.HumanSetLabels(n, "alex", "istari:ready")
	f.run(t)
	if tk := f.ticket(t, n); tk.State != core.TicketReady || tk.Priority != "P1" {
		t.Fatalf("ticket = %s %q, want ready and P1 from its table", tk.State, tk.Priority)
	}
	if p := f.prioritized(t, n); len(p) != 1 || p[0].Actor != "human:carol" {
		t.Fatalf("prioritized events = %+v, want one by the author human:carol", p)
	}
	if !slices.Contains(f.gh.Issue(n).Labels, "priority:P1") {
		t.Fatalf("labels = %v, want priority:P1", f.gh.Issue(n).Labels)
	}
}

func TestATicketAdmittedByHandGetsItsTablesPriority(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Add limits", conforming, "carol") // carol is not trusted
	f.run(t)
	if tk := f.ticket(t, n); tk.Priority != "" {
		t.Fatalf("priority = %q before admission, want none", tk.Priority)
	}
	if err := f.sync.Ready(ctx, "alex", ref(n)); err != nil {
		t.Fatal(err)
	}
	if tk := f.ticket(t, n); tk.Priority != "P1" {
		t.Fatalf("priority = %q, want P1 from its table", tk.Priority)
	}
	if p := f.prioritized(t, n); len(p) != 1 || p[0].Actor != "human:carol" {
		t.Fatalf("prioritized events = %+v, want one by the author human:carol", p)
	}
	if !slices.Contains(f.gh.Issue(n).Labels, "priority:P1") {
		t.Fatalf("labels = %v, want priority:P1", f.gh.Issue(n).Labels)
	}
}
