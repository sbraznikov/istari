package github_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
)

var ctx = context.Background()

func newProvider(t *testing.T) (*github.Provider, *fakeGitHub) {
	t.Helper()
	fake, srv := newFakeGitHub(t)
	return github.New(github.Options{BaseURL: srv.URL, Token: fake.token}), fake
}

func ref(n int) tickets.Ref { return tickets.Ref{Provider: "gh", Repo: "acme/web", Number: n} }

func stateLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if strings.HasPrefix(l, "istari:") && !strings.HasPrefix(l, "istari:hold:") {
			out = append(out, l)
		}
	}
	return out
}

func TestCreateMakesAnIssueInStateNewWithItsLabels(t *testing.T) {
	p, fake := newProvider(t)
	ticket, err := p.Create(ctx, tickets.Draft{Repo: "acme/web", Title: "Add rate limits", Body: "## Context\n…", Labels: []string{"bug", "size:S"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ticket.Ref.String() != "gh:acme/web#1" || ticket.State.State != core.TicketNew {
		t.Fatalf("created %s in %s, want gh:acme/web#1 in new", ticket.Ref, ticket.State.State)
	}
	issue := fake.snapshot(1)
	slices.Sort(issue.Labels)
	if want := []string{"bug", "istari:new", "size:S"}; !slices.Equal(issue.Labels, want) {
		t.Fatalf("issue labels = %v, want %v", issue.Labels, want)
	}
	// Missing labels were created in the repository, istari's with a description.
	if l, ok := fake.labels["istari:new"]; !ok || l.Color == "" || l.Description == "" {
		t.Fatalf("label istari:new = %+v, %v; want it created with a colour and a description", l, ok)
	}
	if _, ok := fake.labels["size:S"]; !ok {
		t.Fatal("label size:S was not created")
	}
}

func TestGetRoundTripsTitleBodyLabelsAndAuthor(t *testing.T) {
	p, fake := newProvider(t)
	n := fake.seed("Flaky test", "Steps:\n1. run\n", "alex", "bug", "istari:in_progress", "istari:hold:paused")
	ticket, err := p.Get(ctx, ref(n))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Title != "Flaky test" || ticket.Body != "Steps:\n1. run\n" || ticket.Author != "alex" {
		t.Fatalf("Get = %+v", ticket)
	}
	if !slices.Equal(ticket.Labels, []string{"bug", "istari:in_progress", "istari:hold:paused"}) {
		t.Fatalf("labels = %v", ticket.Labels)
	}
	if ticket.State != (tickets.State{State: core.TicketInProgress, Hold: "paused"}) {
		t.Fatalf("state = %+v", ticket.State)
	}
	if _, err := p.Get(ctx, ref(404)); !errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("Get of a missing issue: %v, want ErrNotFound", err)
	}
}

func TestSetStateLeavesExactlyOneStateLabel(t *testing.T) {
	p, fake := newProvider(t)
	// A human left two state labels; istari's write repairs that.
	n := fake.seed("t", "", "alex", "bug", "istari:new", "istari:ready")
	for _, s := range []core.TicketState{core.TicketPlanning, core.TicketInProgress, core.TicketInReview} {
		if err := p.SetState(ctx, ref(n), tickets.State{State: s}); err != nil {
			t.Fatalf("SetState %s: %v", s, err)
		}
		issue := fake.snapshot(n)
		if got := stateLabels(issue.Labels); !slices.Equal(got, []string{"istari:" + string(s)}) {
			t.Fatalf("after SetState %s the state labels are %v", s, got)
		}
		if !slices.Contains(issue.Labels, "bug") {
			t.Fatalf("SetState removed a label that is not istari's: %v", issue.Labels)
		}
	}
}

func TestDoneClosesAsCompletedAndCanceledAsNotPlanned(t *testing.T) {
	p, fake := newProvider(t)
	done := fake.seed("a", "", "alex", "istari:awaiting_approval")
	canceled := fake.seed("b", "", "alex", "istari:in_progress")
	if err := p.SetState(ctx, ref(done), tickets.State{State: core.TicketDone}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, ref(canceled), tickets.State{State: core.TicketCanceled}); err != nil {
		t.Fatal(err)
	}
	if i := fake.snapshot(done); i.State != "closed" || i.StateReason != "completed" {
		t.Fatalf("done issue = %s/%s, want closed/completed", i.State, i.StateReason)
	}
	if i := fake.snapshot(canceled); i.State != "closed" || i.StateReason != "not_planned" {
		t.Fatalf("canceled issue = %s/%s, want closed/not_planned", i.State, i.StateReason)
	}
	// Reopening a canceled ticket (T14) reopens the issue.
	if err := p.SetState(ctx, ref(canceled), tickets.State{State: core.TicketNew}); err != nil {
		t.Fatal(err)
	}
	if i := fake.snapshot(canceled); i.State != "open" || !slices.Equal(stateLabels(i.Labels), []string{"istari:new"}) {
		t.Fatalf("reopened issue = %s %v, want open with istari:new", i.State, i.Labels)
	}
}

func TestAHoldLabelComesAndGoesWithoutTouchingTheStateLabel(t *testing.T) {
	p, fake := newProvider(t)
	n := fake.seed("t", "", "alex", "istari:in_review")
	if err := p.SetState(ctx, ref(n), tickets.State{State: core.TicketInReview, Hold: "needs_input"}); err != nil {
		t.Fatal(err)
	}
	if i := fake.snapshot(n); !slices.Equal(i.Labels, []string{"istari:in_review", "istari:hold:needs_input"}) {
		t.Fatalf("held labels = %v", i.Labels)
	}
	if err := p.SetState(ctx, ref(n), tickets.State{State: core.TicketInReview}); err != nil {
		t.Fatal(err)
	}
	if i := fake.snapshot(n); !slices.Equal(i.Labels, []string{"istari:in_review"}) {
		t.Fatalf("after clearing the hold the labels are %v", i.Labels)
	}
}

func TestListFiltersByLabelsAcrossPagesAndSkipsPullRequests(t *testing.T) {
	p, fake := newProvider(t)
	fake.pageSize = 2
	for i := range 5 {
		fake.seed("ready", "", "alex", "istari:ready")
		_ = i
	}
	fake.seed("other", "", "alex", "bug")
	pr := fake.seed("a PR", "", "alex", "istari:ready")
	fake.issues[pr].PullRequest = true
	got, err := p.List(ctx, tickets.Query{Repo: "acme/web", Labels: []string{"istari:ready"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("List = %d tickets, want the 5 ready issues", len(got))
	}
	for _, tk := range got {
		if tk.State.State != core.TicketReady {
			t.Fatalf("listed %s in %s", tk.Ref, tk.State.State)
		}
	}
}

func TestCommentAndLink(t *testing.T) {
	p, fake := newProvider(t)
	parent := fake.seed("parent", "", "alex", "istari:in_progress")
	child := fake.seed("child", "", "alex", "istari:new")
	c, err := p.Comment(ctx, ref(parent), "Plan published.")
	if err != nil || c.ID == 0 || !strings.Contains(c.URL, "issuecomment") {
		t.Fatalf("Comment = %+v, %v", c, err)
	}
	if err := p.Link(ctx, ref(parent), tickets.Link{Kind: tickets.LinkChild, Child: ref(child)}); err != nil {
		t.Fatalf("Link child: %v", err)
	}
	if got := fake.subIssues[parent]; len(got) != 1 || got[0] != fake.issues[child].ID {
		t.Fatalf("sub-issues of the parent = %v, want the child's id %d", got, fake.issues[child].ID)
	}
	if err := p.Link(ctx, ref(parent), tickets.Link{Kind: tickets.LinkPullRequest, URL: "https://github.com/acme/web/pull/9"}); err != nil {
		t.Fatal(err)
	}
	if cs := fake.comments[parent]; len(cs) != 2 || !strings.Contains(cs[1], "https://github.com/acme/web/pull/9") {
		t.Fatalf("comments = %q, want the pull request linked in one", cs)
	}
	if caps := p.Capabilities(); !caps.SubIssues || caps.CustomStates {
		t.Fatalf("Capabilities = %+v", caps)
	}
}

func TestAWrongTokenIsAnError(t *testing.T) {
	_, srv := newFakeGitHub(t)
	p := github.New(github.Options{BaseURL: srv.URL, Token: "wrong"})
	if _, err := p.Get(ctx, ref(1)); err == nil || errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("Get with a wrong token: %v, want an authentication error", err)
	}
}
