package tickets_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
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
	if len(tr) != 2 || tr[1].Trigger != "accept" || tr[1].Actor != "system" {
		t.Fatalf("transitions = %+v, want create then accept by the system", tr)
	}
	if got := istariLabels(f.gh.Issue(n).Labels); !slices.Equal(got, []string{"istari:ready"}) {
		t.Fatalf("labels = %v", got)
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
	if tr := f.transitions(t, n); tr[len(tr)-1].Actor != "human:alex" {
		t.Fatalf("transitions = %+v, want the accept by human:alex", tr)
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
