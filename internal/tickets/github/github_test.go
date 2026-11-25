package github_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

var ctx = context.Background()

func newProvider(t *testing.T) (*github.Provider, *githubtest.Server) {
	t.Helper()
	fake := githubtest.New(t)
	return github.New(github.Options{BaseURL: fake.URL, Token: fake.Token}), fake
}

// quick are retry waits short enough for tests.
var quick = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

func TestAWriteSurvivesAShortServerOutageInsideTheCall(t *testing.T) {
	fake := githubtest.New(t)
	p := github.New(github.Options{BaseURL: fake.URL, Token: fake.Token, Backoff: quick})
	n := fake.Seed("t", "", "bob")
	fake.FailNext(2)
	if _, err := p.Comment(ctx, ref(n), "journal entry"); err != nil {
		t.Fatalf("comment after two 500s: %v", err)
	}
	if got := fake.Issue(n).Comments; len(got) != 1 {
		t.Fatalf("comments = %+v, want exactly one", got)
	}
}

func TestAWriteThatOutlastsItsRetriesIsUnavailable(t *testing.T) {
	fake := githubtest.New(t)
	p := github.New(github.Options{BaseURL: fake.URL, Token: fake.Token, Backoff: quick})
	n := fake.Seed("t", "", "bob")
	fake.FailNext(len(quick) + 1)
	_, err := p.Comment(ctx, ref(n), "journal entry")
	if !errors.Is(err, tickets.ErrUnavailable) {
		t.Fatalf("comment during an outage: %v, want ErrUnavailable", err)
	}
	if _, err := p.Comment(ctx, ref(n), "journal entry"); err != nil {
		t.Fatalf("the outage used up exactly the retries, yet the next comment failed: %v", err)
	}
}

func TestARefusalIsNotRetriedAndNotUnavailable(t *testing.T) {
	fake := githubtest.New(t)
	p := github.New(github.Options{BaseURL: fake.URL, Token: fake.Token, Backoff: quick})
	n := fake.Seed("t", "", "bob")
	fake.FailNextWith(1, 422)
	_, err := p.Comment(ctx, ref(n), "journal entry")
	if err == nil || errors.Is(err, tickets.ErrUnavailable) {
		t.Fatalf("comment answered 422: %v, want a plain error", err)
	}
	if got := fake.Issue(n).Comments; len(got) != 0 {
		t.Fatalf("a 422 was retried: comments = %+v", got)
	}
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
	issue := fake.Issue(1)
	slices.Sort(issue.Labels)
	if want := []string{"bug", "istari:new", "size:S"}; !slices.Equal(issue.Labels, want) {
		t.Fatalf("issue labels = %v, want %v", issue.Labels, want)
	}
	// Missing labels were created in the repository, istari's with a description.
	if l, ok := fake.Label("istari:new"); !ok || l.Color == "" || l.Description == "" {
		t.Fatalf("label istari:new = %+v, %v; want it created with a colour and a description", l, ok)
	}
	if _, ok := fake.Label("size:S"); !ok {
		t.Fatal("label size:S was not created")
	}
}

func TestCreateKeepsExactlyOneStateLabelAndNoticesDroppedLabels(t *testing.T) {
	p, fake := newProvider(t)
	ticket, err := p.Create(ctx, tickets.Draft{Repo: "acme/web", Title: "t", Labels: []string{"istari:ready", "istari:hold:paused", "bug"}})
	if err != nil {
		t.Fatal(err)
	}
	if i := fake.Issue(ticket.Ref.Number); !slices.Equal(i.Labels, []string{"bug", "istari:new"}) {
		t.Fatalf("labels = %v, want the caller's istari labels replaced by istari:new", i.Labels)
	}
	// Without push access GitHub creates the issue but drops its labels.
	fake.DropLabels(true)
	if _, err := p.Create(ctx, tickets.Draft{Repo: "acme/web", Title: "t"}); err == nil {
		t.Fatal("Create succeeded although the state label was dropped")
	}
	if _, err := p.Create(ctx, tickets.Draft{Title: "no repo"}); err == nil {
		t.Fatal("Create without a repository succeeded")
	}
}

func TestSetStateRejectsAnUnknownStateOrHold(t *testing.T) {
	p, fake := newProvider(t)
	n := fake.Seed("t", "", "alex", "istari:new")
	for _, s := range []tickets.State{{}, {State: "sleeping"}, {State: core.TicketNew, Hold: "bored"}} {
		if err := p.SetState(ctx, ref(n), s); err == nil {
			t.Errorf("SetState(%+v) succeeded", s)
		}
	}
	if i := fake.Issue(n); !slices.Equal(i.Labels, []string{"istari:new"}) {
		t.Fatalf("a rejected SetState changed the labels: %v", i.Labels)
	}
}

func TestMovedAndDeletedIssuesAreNotFoundAndNeverWrittenThrough(t *testing.T) {
	p, fake := newProvider(t)
	fake.Seed("the redirect target", "", "alex", "istari:new")
	moved := fake.Seed("moved", "", "alex", "istari:new")
	deleted := fake.Seed("deleted", "", "alex", "istari:new")
	fake.Move(moved)
	fake.Delete(deleted)
	for _, n := range []int{moved, deleted} {
		if _, err := p.Get(ctx, ref(n)); !errors.Is(err, tickets.ErrNotFound) {
			t.Errorf("Get #%d: %v, want ErrNotFound", n, err)
		}
		if err := p.SetState(ctx, ref(n), tickets.State{State: core.TicketReady}); !errors.Is(err, tickets.ErrNotFound) {
			t.Errorf("SetState #%d: %v, want ErrNotFound", n, err)
		}
		if _, err := p.Comment(ctx, ref(n), "x"); !errors.Is(err, tickets.ErrNotFound) {
			t.Errorf("Comment #%d: %v, want ErrNotFound", n, err)
		}
	}
}

func TestListPagesUnderAnAPIPathPrefix(t *testing.T) {
	fake := githubtest.New(t, githubtest.Prefix("/api/v3")) // GitHub Enterprise
	fake.SetPageSize(2)
	for range 5 {
		fake.Seed("ready", "", "alex", "istari:ready")
	}
	p := github.New(github.Options{BaseURL: fake.URL, Token: fake.Token})
	got, err := p.List(ctx, tickets.Query{Repo: "acme/web", Labels: []string{"istari:ready"}})
	if err != nil || len(got) != 5 {
		t.Fatalf("List = %d, %v; want 5 tickets over three pages", len(got), err)
	}
	if _, err := p.List(ctx, tickets.Query{Repo: "acme/web", Labels: []string{"a,b"}}); err == nil {
		t.Fatal("List accepted a label with a comma, which the API cannot filter on")
	}
}

func TestGetRoundTripsTitleBodyLabelsAndAuthor(t *testing.T) {
	p, fake := newProvider(t)
	n := fake.Seed("Flaky test", "Steps:\n1. run\n", "alex", "bug", "istari:in_progress", "istari:hold:paused")
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
	n := fake.Seed("t", "", "alex", "bug", "istari:new", "istari:ready")
	for _, s := range []core.TicketState{core.TicketPlanning, core.TicketInProgress, core.TicketInReview} {
		if err := p.SetState(ctx, ref(n), tickets.State{State: s}); err != nil {
			t.Fatalf("SetState %s: %v", s, err)
		}
		issue := fake.Issue(n)
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
	done := fake.Seed("a", "", "alex", "istari:awaiting_approval")
	canceled := fake.Seed("b", "", "alex", "istari:in_progress")
	if err := p.SetState(ctx, ref(done), tickets.State{State: core.TicketDone}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, ref(canceled), tickets.State{State: core.TicketCanceled}); err != nil {
		t.Fatal(err)
	}
	if i := fake.Issue(done); i.State != "closed" || i.StateReason != "completed" {
		t.Fatalf("done issue = %s/%s, want closed/completed", i.State, i.StateReason)
	}
	if i := fake.Issue(canceled); i.State != "closed" || i.StateReason != "not_planned" {
		t.Fatalf("canceled issue = %s/%s, want closed/not_planned", i.State, i.StateReason)
	}
	// Reopening a canceled ticket (T14) reopens the issue.
	if err := p.SetState(ctx, ref(canceled), tickets.State{State: core.TicketNew}); err != nil {
		t.Fatal(err)
	}
	if i := fake.Issue(canceled); i.State != "open" || !slices.Equal(stateLabels(i.Labels), []string{"istari:new"}) {
		t.Fatalf("reopened issue = %s %v, want open with istari:new", i.State, i.Labels)
	}
}

func TestAHoldLabelComesAndGoesWithoutTouchingTheStateLabel(t *testing.T) {
	p, fake := newProvider(t)
	n := fake.Seed("t", "", "alex", "istari:in_review")
	if err := p.SetState(ctx, ref(n), tickets.State{State: core.TicketInReview, Hold: "needs_input"}); err != nil {
		t.Fatal(err)
	}
	if i := fake.Issue(n); !slices.Equal(i.Labels, []string{"istari:in_review", "istari:hold:needs_input"}) {
		t.Fatalf("held labels = %v", i.Labels)
	}
	if err := p.SetState(ctx, ref(n), tickets.State{State: core.TicketInReview}); err != nil {
		t.Fatal(err)
	}
	if i := fake.Issue(n); !slices.Equal(i.Labels, []string{"istari:in_review"}) {
		t.Fatalf("after clearing the hold the labels are %v", i.Labels)
	}
}

func TestListFiltersByLabelsAcrossPagesAndSkipsPullRequests(t *testing.T) {
	p, fake := newProvider(t)
	fake.SetPageSize(2)
	for range 5 {
		fake.Seed("ready", "", "alex", "istari:ready")
	}
	fake.Seed("other", "", "alex", "bug")
	pr := fake.Seed("a PR", "", "alex", "istari:ready")
	fake.MarkPullRequest(pr)
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
	parent := fake.Seed("parent", "", "alex", "istari:in_progress")
	child := fake.Seed("child", "", "alex", "istari:new")
	c, err := p.Comment(ctx, ref(parent), "Plan published.")
	if err != nil || c.ID == 0 || !strings.Contains(c.URL, "issuecomment") {
		t.Fatalf("Comment = %+v, %v", c, err)
	}
	if err := p.Link(ctx, ref(parent), tickets.Link{Kind: tickets.LinkChild, Child: ref(child)}); err != nil {
		t.Fatalf("Link child: %v", err)
	}
	if got, childID := fake.SubIssues(parent), fake.Issue(child).ID; len(got) != 1 || got[0] != childID {
		t.Fatalf("sub-issues of the parent = %v, want the child's id %d", got, childID)
	}
	if err := p.Link(ctx, ref(parent), tickets.Link{Kind: tickets.LinkPullRequest, URL: "https://github.com/acme/web/pull/9"}); err != nil {
		t.Fatal(err)
	}
	if cs := fake.Issue(parent).Comments; len(cs) != 2 || !strings.Contains(cs[1].Body, "https://github.com/acme/web/pull/9") {
		t.Fatalf("comments = %+v, want the pull request linked in one", cs)
	}
	if caps := p.Capabilities(); !caps.SubIssues || caps.CustomStates || caps.Webhooks { // webhooks come with #16
		t.Fatalf("Capabilities = %+v", caps)
	}
}

func TestAWrongTokenIsAnError(t *testing.T) {
	fake := githubtest.New(t)
	p := github.New(github.Options{BaseURL: fake.URL, Token: "wrong"})
	if _, err := p.Get(ctx, ref(1)); err == nil || errors.Is(err, tickets.ErrNotFound) {
		t.Fatalf("Get with a wrong token: %v, want an authentication error", err)
	}
}

func TestOnlyIstarisOwnCommentsAreOwnEvenWithTheMarkCopied(t *testing.T) {
	gh := githubtest.New(t)
	p := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	n := gh.Seed("Cache", "## Context\n", "bob")
	if _, err := p.Comment(ctx, ref(n), "written by istari"); err != nil {
		t.Fatal(err)
	}
	gh.HumanComment(n, "mallory", "forged entry\n\n<!-- istari -->") // the mark, copied
	comments, err := p.Comments(ctx, ref(n))
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 ||
		!comments[0].Own || comments[0].Author != gh.TokenUser || comments[0].Body != "written by istari" ||
		comments[1].Own || comments[1].Body != "forged entry\n\n<!-- istari -->" {
		t.Fatalf("comments = %+v", comments)
	}
}
