package tickets_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

const planYAML = `
repo: acme/web
tickets:
  - key: parent
    title: Reports in the new format
    body: |
      ## Context

      Two parts.
  - key: core
    title: Add the format to the core
    parent: parent
    body: |
      ## Context

      The core part.
  - key: use
    title: Use the new format
    parent: parent
    needs: [core]
    body: |
      ## Context

      Uses #core once it is done.
`

func TestAValidPlanIsCreatedInOrderWithKeysReplacedAndChildrenLinked(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	tracker := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	p, err := tickets.ParsePlan([]byte(planYAML))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := tickets.CreatePlan(ctx, tracker, p, "acme/web")
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if len(refs) != 3 || refs["core"].Number >= refs["use"].Number || refs["parent"].Number >= refs["core"].Number {
		t.Fatalf("refs = %v, want parent, core, use in that order", refs)
	}
	use := gh.Issue(refs["use"].Number)
	core := "#" + strconv.Itoa(refs["core"].Number)
	if strings.Contains(use.Body, "#core") || !strings.Contains(use.Body, "Uses "+core+" once") || !strings.Contains(use.Body, "- Needs: "+core) {
		t.Fatalf("the body of use:\n%s", use.Body)
	}
	if !strings.Contains(use.Body, "- Parent: #"+strconv.Itoa(refs["parent"].Number)) {
		t.Fatalf("the body of use names no parent:\n%s", use.Body)
	}
	if children := gh.SubIssues(refs["parent"].Number); len(children) != 2 {
		t.Fatalf("sub-issues of the parent = %v, want core and use", children)
	}
}

func TestAPlanWithACycleOrAnUnknownKeyCreatesNothing(t *testing.T) {
	t.Parallel()
	for name, plan := range map[string]string{
		"cycle":       "tickets:\n  - key: a\n    title: A\n    needs: [b]\n  - key: b\n    title: B\n    needs: [a]\n",
		"unknown key": "tickets:\n  - key: a\n    title: A\n    needs: [nowhere]\n",
		"missing ref": "tickets:\n  - key: a\n    title: A\n    needs: [\"gh:acme/web#404\"]\n",
		"twice":       "tickets:\n  - key: a\n    title: A\n  - key: a\n    title: B\n",
	} {
		gh := githubtest.New(t)
		tracker := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
		p, err := tickets.ParsePlan([]byte(plan))
		if err == nil {
			_, err = tickets.CreatePlan(ctx, tracker, p, "acme/web")
		}
		if err == nil {
			t.Errorf("%s: the plan was accepted", name)
		}
		if n := gh.IssueCount(); n != 0 {
			t.Errorf("%s: %d issues were created", name, n)
		}
	}
}

func TestATicketAnAgentCreatesForAnAdmittedTicketIsAdmittedAndSharesItsBudget(t *testing.T) {
	t.Parallel()
	f := newSync(t, "alex") // alex admits; carol, the authors of the new tickets, is not trusted
	parent := f.gh.Seed("Parent", conforming, "bob")
	f.gh.SetPermission("alex", "write")
	f.gh.HumanSetLabels(parent, "alex", "istari:ready")
	f.run(t)
	child := f.gh.Seed("Found while working", conforming, "carol")
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: child}
	if err := f.sync.Created(ctx, "agent:implementer-1", []tickets.Ref{ref}, ghRef(parent)); err != nil {
		t.Fatalf("Created: %v", err)
	}
	tk := f.ticket(t, child)
	if tk.State != "ready" || tk.BudgetOf != ghRef(parent) {
		t.Fatalf("the agent's ticket = %s, budget of %q; want ready, sharing %s's budget", tk.State, tk.BudgetOf, ghRef(parent))
	}
	// Without an admitted parent, the ticket waits for admission as any other.
	other := f.gh.Seed("A human's", conforming, "carol")
	if err := f.sync.Created(ctx, "human:carol", []tickets.Ref{{Provider: "gh", Repo: githubtest.Repo, Number: other}}, ""); err != nil {
		t.Fatal(err)
	}
	if tk := f.ticket(t, other); tk.State != "new" || tk.BudgetOf != "" {
		t.Fatalf("a ticket without an admitted parent = %+v", tk)
	}
}

func TestTheChildrenOfASplitTicketGetItsPriorityUnlessTheirTableSaysOtherwise(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	parent := f.gh.Seed("Parent", withTablePriority("P0"), "alex")
	f.run(t)
	plain := f.gh.Seed("Child", withTablePriority(""), "alex")
	own := f.gh.Seed("Child with its own", withTablePriority("P2"), "alex")
	refs := []tickets.Ref{{Provider: "gh", Repo: githubtest.Repo, Number: plain}, {Provider: "gh", Repo: githubtest.Repo, Number: own}}
	if err := f.sync.Created(ctx, "agent:planner-1", refs, ghRef(parent)); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if tk := f.ticket(t, plain); tk.State != "ready" || tk.Priority != "P0" {
		t.Fatalf("child without a table priority = %s %q, want ready and the parent's P0", tk.State, tk.Priority)
	}
	if !slices.Contains(f.gh.Issue(plain).Labels, "priority:P0") {
		t.Fatalf("labels = %v, want priority:P0", f.gh.Issue(plain).Labels)
	}
	if tk := f.ticket(t, own); tk.Priority != "P2" {
		t.Fatalf("child whose table says P2 = %q, want P2", tk.Priority)
	}
}

func TestATicketIstariCreatesGetsItsTablesPriorityAtOnce(t *testing.T) {
	t.Parallel()
	f := newSync(t)
	n := f.gh.Seed("Created by istari ticket create", conforming, "carol") // not trusted: it stays new
	if err := f.sync.Created(ctx, "human:carol", []tickets.Ref{{Provider: "gh", Repo: githubtest.Repo, Number: n}}, ""); err != nil {
		t.Fatal(err)
	}
	if tk := f.ticket(t, n); tk.State != "new" || tk.Priority != "P1" {
		t.Fatalf("ticket = %s %q, want new and P1 from its table", tk.State, tk.Priority)
	}
	if !slices.Contains(f.gh.Issue(n).Labels, "priority:P1") {
		t.Fatalf("labels = %v, want priority:P1", f.gh.Issue(n).Labels)
	}
}

func TestKeysAreReplacedOnlyAsWholeReferencesOutsideCode(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	tracker := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	p, err := tickets.ParsePlan([]byte("tickets:\n  - key: base\n    title: Base\n  - key: base-x\n    title: Base X\n  - key: top\n    title: Top\n    needs: [base, base-x]\n    body: \"See #base and #base-x, not #basement.\\n\\n```\\n#base stays\\n```\\n\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := tickets.CreatePlan(ctx, tracker, p, "acme/web")
	if err != nil {
		t.Fatal(err)
	}
	body := gh.Issue(refs["top"].Number).Body
	want := "See #" + strconv.Itoa(refs["base"].Number) + " and #" + strconv.Itoa(refs["base-x"].Number) + ", not #basement."
	if !strings.Contains(body, want) || !strings.Contains(body, "#base stays") {
		t.Fatalf("body:\n%s\nwant %q and the code block kept", body, want)
	}
}

func TestABodyNamingAKeyCreatedLaterIsRefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	tracker := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	p, _ := tickets.ParsePlan([]byte("tickets:\n  - key: first\n    title: First\n    body: \"Before #second.\"\n  - key: second\n    title: Second\n    needs: [first]\n"))
	if _, err := tickets.CreatePlan(ctx, tracker, p, "acme/web"); err == nil || !strings.Contains(err.Error(), "second") {
		t.Fatalf("CreatePlan = %v, want a refusal naming #second", err)
	}
	if gh.IssueCount() != 0 {
		t.Fatalf("%d issues were created", gh.IssueCount())
	}
}
