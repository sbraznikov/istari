package runner_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/tickets/standard"
)

const rawTicket = "We need rate limits on the export API, see `internal/export/limit.go`.\n"

type refineFixture struct {
	gh      *githubtest.Server
	tickets *core.Tickets
	refines *runner.Refines
	run     runner.Run
	n       int
}

func newRefine(t *testing.T) *refineFixture {
	t.Helper()
	gh := githubtest.New(t)
	gh.SetFile("internal/export/limit.go", "package export")
	n := gh.Seed("Rate limits", rawTicket, "bob")
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	adapter := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	k := core.NewTickets(s, core.TicketOptions{})
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: n}
	if _, err := k.Create(ctx, "human:bob", ref.String(), "web"); err != nil {
		t.Fatal(err)
	}
	r := runner.NewRefines(adapter, journal.New(adapter, s, nil), k, standard.ForTracker{Tracker: adapter, DefaultPriority: "P2"})
	return &refineFixture{gh: gh, tickets: k, refines: r, n: n, run: runner.Run{ID: "r_1", Ticket: ref.String(), Role: "refiner", Profile: "refiner-1"}}
}

func standardBody(original string) string {
	return "## Context\n\nThe export API has no rate limits.\n\n## What needs to be done\n\n- Add a limiter in `internal/export/limit.go`.\n\n" +
		"## Acceptance criteria\n\n- [ ] A burst over the limit is refused.\n\n## Out of scope\n\n- Per-user limits.\n\n## Dependencies\n\n- Needs: none\n\n" +
		"---\n\n| Type | Size | Priority | Repository | Spec |\n|---|---|---|---|---|\n| feature | S | P2 | `acme/web` | `docs/design.md`, section 4 |\n\n" +
		"<details><summary>Original text</summary>\n\n" + original + "\n</details>\n"
}

func TestAMalformedRefinerResultIsRejected(t *testing.T) {
	for name, data := range map[string]string{
		"not yaml":        "verdict: [",
		"no verdict":      "size: S\n",
		"unknown field":   "verdict: meets\nsize: S\nmood: great\n",
		"bad size":        "verdict: meets\nsize: huge\n",
		"rewrite no body": "verdict: rewrite\nsize: S\n",
		"no questions":    "verdict: needs_input\nsize: S\n",
	} {
		if _, err := runner.ParseRefine([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestARewriteMustKeepTheOriginalByteForByte(t *testing.T) {
	f := newRefine(t)
	changed := standardBody(strings.Replace(rawTicket, "rate limits", "limits", 1))
	err := f.refines.Apply(ctx, f.run, runner.RefineResult{Verdict: "rewrite", Size: "S", Body: changed, Changes: []string{"structure"}})
	if err == nil || !strings.Contains(err.Error(), "original") {
		t.Fatalf("a rewrite that changed the original: %v", err)
	}
	if body := f.gh.Issue(f.n).Body; body != rawTicket {
		t.Fatalf("the rejected rewrite reached the ticket:\n%s", body)
	}
}

func TestAKeptOriginalRewriteIsWrittenCheckedAndExplained(t *testing.T) {
	f := newRefine(t)
	err := f.refines.Apply(ctx, f.run, runner.RefineResult{Verdict: "rewrite", Size: "S", Body: standardBody(rawTicket),
		Changes: []string{"Put the text into the standard's sections"}, Assumptions: []string{"Per-user limits are out of scope"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	issue := f.gh.Issue(f.n)
	if !strings.Contains(issue.Body, "## Acceptance criteria") || !strings.Contains(issue.Body, rawTicket) {
		t.Fatalf("body:\n%s", issue.Body)
	}
	comments := ""
	for _, c := range issue.Comments {
		comments += c.Body + "\n"
	}
	if !strings.Contains(comments, "Put the text into the standard's sections") || !strings.Contains(comments, "Per-user limits are out of scope") {
		t.Fatalf("comments:\n%s", comments)
	}
	if strings.Contains(comments, "Still not meeting") {
		t.Fatalf("the rewritten ticket does not pass the structural check:\n%s", comments)
	}
	if !contains(issue.Labels, "size:S") {
		t.Fatalf("labels = %v, want size:S", issue.Labels)
	}
	tk, _ := f.tickets.Get(ctx, f.run.Ticket)
	if tk.State != core.TicketNew || tk.Hold != "" {
		t.Fatalf("after a rewrite the ticket is %s, hold %q; admission is not the refiner's", tk.State, tk.Hold)
	}
}

func TestMissingInformationBecomesAQuestionAndTheHold(t *testing.T) {
	f := newRefine(t)
	if err := f.refines.Apply(ctx, f.run, runner.RefineResult{Verdict: "needs_input", Size: "S", Questions: []string{"Which endpoints count as export?"}}); err != nil {
		t.Fatal(err)
	}
	if tk, _ := f.tickets.Get(ctx, f.run.Ticket); tk.Hold != "needs_input" {
		t.Fatalf("hold = %q, want needs_input", tk.Hold)
	}
	if last := f.gh.Issue(f.n).Comments; len(last) == 0 || !strings.Contains(last[len(last)-1].Body, "Which endpoints count as export?") {
		t.Fatalf("comments = %+v, want the question", last)
	}
}

func TestAnXLTicketIsMarkedForASplit(t *testing.T) {
	f := newRefine(t)
	if err := f.refines.Apply(ctx, f.run, runner.RefineResult{Verdict: "meets", Size: "XL"}); err != nil {
		t.Fatal(err)
	}
	issue := f.gh.Issue(f.n)
	if !contains(issue.Labels, "size:XL") || !strings.Contains(issue.Comments[len(issue.Comments)-1].Body, "split") {
		t.Fatalf("labels %v, comments %+v; want XL marked for a split", issue.Labels, issue.Comments)
	}
}

func TestADuplicateBecomesADecisionCard(t *testing.T) {
	f := newRefine(t)
	card := &journal.DecisionEntry{Problem: "#1 may duplicate #2", State: "Both add export limits.", Recommendation: "A: close as a duplicate", Recommended: "A", Status: "proposed",
		Options: []journal.Option{{Letter: "A", Gist: "Close as a duplicate", Consequences: "c", Size: "XS", Risks: "r", Changes: "none"}, {Letter: "B", Gist: "Keep both", Consequences: "c", Size: "XS", Risks: "r", Changes: "needs"}}}
	if err := f.refines.Apply(ctx, f.run, runner.RefineResult{Verdict: "duplicate", Size: "S", Card: card}); err != nil {
		t.Fatal(err)
	}
	if tk, _ := f.tickets.Get(ctx, f.run.Ticket); tk.Hold != "needs_input" {
		t.Fatalf("hold = %q, want needs_input until a human chooses", tk.Hold)
	}
	if c := f.gh.Issue(f.n).Comments; !strings.Contains(c[len(c)-1].Body, "**Decision** (proposed)") {
		t.Fatalf("comments = %+v, want the card", c)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
