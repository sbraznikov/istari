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

type planFixture struct {
	*wsFixture
	plans *runner.Plans
	run   runner.Run
	ref   tickets.Ref
}

func newPlans(t *testing.T) *planFixture {
	t.Helper()
	f := newWorkspaces(t)
	seed := filepath.Join(f.root, "seed")
	write(t, filepath.Join(seed, "cache", "cache.go"), "package cache\n")
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "cache")
	gitIn(t, seed, "push", "-q", "origin", "main")
	tk := f.ticket(t, 31, "Add a cache")
	ws := runner.NewWorkspaces(f.store, runner.WorkspaceOptions{DataDir: f.data, Server: "file://" + filepath.Join(f.root, "remotes")})
	if _, err := ws.Ensure(ctx, tk); err != nil { // fetches main into istari's clone
		t.Fatal(err)
	}
	ref, _ := tickets.ParseRef(tk.Ref)
	return &planFixture{wsFixture: f, ref: ref,
		plans: runner.NewPlans(ws, journal.New(nil, f.store, nil), f.tickets, nil),
		run:   runner.Run{ID: "r_p", Ticket: tk.Ref, Role: "planner", Profile: "planner-1"}}
}

const goodPlan = `
summary: A cache in front of the store
slices: [the cache layer, the race test]
seams:
  - name: Cache.Get
    catches: stale reads
    misses: eviction order
zone: [cache/cache.go, cache/cache_test.go (new)]
`

func TestAPlanWithoutSeamsOrZoneIsRejected(t *testing.T) {
	t.Parallel()
	for name, plan := range map[string]string{
		"no seams":      "summary: s\nslices: [a]\nzone: [cache/cache.go]\n",
		"no zone":       "summary: s\nslices: [a]\nseams: [{name: X, catches: c, misses: m}]\n",
		"seam half":     "summary: s\nslices: [a]\nzone: [cache/cache.go]\nseams: [{name: X, catches: c}]\n",
		"no slices":     "summary: s\nzone: [cache/cache.go]\nseams: [{name: X, catches: c, misses: m}]\n",
		"unknown field": goodPlan + "mood: calm\n",
	} {
		if _, err := runner.ParsePlanResult([]byte(plan)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAPlanWithAnUnquotedColonIsRefusedWithItsLineAndTheFix(t *testing.T) {
	t.Parallel()
	plan := "slices: [a]\nsummary: A cache: in front of the store\n"
	_, err := runner.ParsePlanResult([]byte(plan))
	refusesUnquotedColon(t, err, 2)
}

func TestAZonePathThatDoesNotExistAndIsNotNewIsRejected(t *testing.T) {
	t.Parallel()
	f := newPlans(t)
	res, err := runner.ParsePlanResult([]byte(strings.Replace(goodPlan, "cache/cache.go,", "cache/missing.go,", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.plans.Check(ctx, f.run, res, runner.PlanSettings{MaxSize: "L"}); err == nil || !strings.Contains(err.Error(), "cache/missing.go") {
		t.Fatalf("Check = %v, want the missing path named", err)
	}
}

func TestAPlanIsJournaledWithItsSeamsAsAnAssumptionWithoutTheGate(t *testing.T) {
	t.Parallel()
	f := newPlans(t)
	res, err := runner.ParsePlanResult([]byte(goodPlan))
	if err != nil {
		t.Fatal(err)
	}
	if res, err = f.plans.Check(ctx, f.run, res, runner.PlanSettings{MaxSize: "L"}); err != nil {
		t.Fatal(err)
	}
	if err := f.plans.Record(ctx, f.run, res, runner.PlanSettings{MaxSize: "L"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := journal.New(nil, f.store, nil).Stored(ctx, f.ref)
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, string(e.Kind))
	}
	if strings.Join(kinds, " ") != "plan assumption" || !strings.Contains(entries[1].Text, "Cache.Get") {
		t.Fatalf("entries = %+v, want the plan and its seams as an assumption", entries)
	}
	if w := journal.Summarize(entries); w.Next != "the cache layer" {
		t.Fatalf("next step = %q, want the first slice", w.Next)
	}
}

func TestAZoneLargerThanTheSizeRevisesTheSize(t *testing.T) {
	t.Parallel()
	f := newPlans(t)
	if _, err := f.tickets.SetSize(ctx, "agent:refiner-1", f.run.Ticket, "XS"); err != nil {
		t.Fatal(err)
	}
	zone := "zone: [a1.go (new), a2.go (new), a3.go (new), a4.go (new)]\n"
	res, _ := runner.ParsePlanResult([]byte(strings.Split(goodPlan, "zone:")[0] + zone))
	res, err := f.plans.Check(ctx, f.run, res, runner.PlanSettings{MaxSize: "L"})
	if err != nil || res.Size != "S" {
		t.Fatalf("Check = %+v, %v; want the size revised to S for four files", res, err)
	}
}

func TestATicketAboveMaxSizeMustBeSplit(t *testing.T) {
	t.Parallel()
	f := newPlans(t)
	if _, err := f.tickets.SetSize(ctx, "agent:refiner-1", f.run.Ticket, "L"); err != nil {
		t.Fatal(err)
	}
	res, _ := runner.ParsePlanResult([]byte(goodPlan))
	if _, err := f.plans.Check(ctx, f.run, res, runner.PlanSettings{MaxSize: "M"}); err == nil || !strings.Contains(err.Error(), "split") {
		t.Fatalf("Check = %v, want a split required", err)
	}
	body := "      body: |\n        " + strings.ReplaceAll(strings.TrimRight(childBody, "\n"), "\n", "\n        ") + "\n"
	split, err := runner.ParsePlanResult([]byte(goodPlan + "split:\n  tickets:\n    - key: one\n      title: First half\n      type: feature\n      size: M\n" + body))
	if err != nil {
		t.Fatal(err)
	}
	f.run.Dir = f.dir(31)
	if _, err := f.plans.Check(ctx, f.run, split, runner.PlanSettings{MaxSize: "M"}); err != nil {
		t.Fatalf("a split plan: %v", err)
	}
}

func TestADirectoryInTheZoneIsRefused(t *testing.T) {
	t.Parallel()
	f := newPlans(t)
	res, _ := runner.ParsePlanResult([]byte(strings.Replace(goodPlan, "cache/cache.go,", "cache/,", 1)))
	if _, err := f.plans.Check(ctx, f.run, res, runner.PlanSettings{MaxSize: "L"}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("Check = %v, want a directory refused: the zone lists files", err)
	}
}

type splitFixture struct {
	gh      *githubtest.Server
	parent  int
	ref     tickets.Ref
	tickets *core.Tickets
	tasks   *core.Tasks
	plans   *runner.Plans
	run     runner.Run
}

// newSplit is a parent ticket in planning, synced from a fake GitHub, and a
// planner's run whose checkout holds cache/cache.go.
func newSplit(t *testing.T) *splitFixture {
	t.Helper()
	gh := githubtest.New(t)
	gh.SetPermission("alex", "write")
	parent := gh.Seed("Too big", "## Context\n\nBig.\n", "alex")
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	adapter := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	k := core.NewTickets(s, core.TicketOptions{})
	tasks := core.NewTasks(s, core.TaskOptions{})
	syncer := tickets.NewSyncer(adapter, k, s, []tickets.SyncRepo{{Repo: githubtest.Repo, Project: "web"}}, tickets.SyncOptions{})
	if _, err := syncer.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: parent}
	for _, tr := range []string{"accept", "start"} {
		if _, err := k.Fire(ctx, "system", ref.String(), tr); err != nil {
			t.Fatal(err)
		}
	}
	plan, _ := tasks.Create(ctx, "system", core.NewTask{Ticket: ref.String(), Project: "web", Role: "planner"})
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cache", "cache.go"), "package cache\n")
	return &splitFixture{gh: gh, parent: parent, ref: ref, tickets: k, tasks: tasks,
		plans: runner.NewPlans(nil, journal.New(nil, s, nil), k, nil).WithSplit(tasks, adapter, syncer.Created),
		run:   runner.Run{ID: "r_s", Ticket: ref.String(), Task: plan.ID, Role: "planner", Profile: "planner-1", Dir: dir}}
}

// childBody is a child ticket's text after the standard, without the table.
const childBody = "## Context\n\nThe cache in `cache/cache.go` is slow.\n\n" +
	"## What needs to be done\n\n- Make it fast.\n\n" +
	"## Acceptance criteria\n\n- [ ] It is fast.\n\n" +
	"## Out of scope\n\n- Eviction.\n\n" +
	"## Dependencies\n\n- Needs: none\n"

// splitPlan is a plan whose zone needs no checkout, split into children.
func splitPlan(children ...tickets.PlanTicket) runner.PlanResult {
	return runner.PlanResult{Summary: "s", Slices: []string{"a"}, Seams: []runner.Seam{{Name: "X", Catches: "c", Misses: "m"}},
		Zone: []string{"x.go (new)"}, Split: &tickets.Plan{Tickets: children}}
}

func TestSplitChildrenOfAP0ParentCarryTheTableAndMeetTheStandard(t *testing.T) {
	f := newSplit(t)
	if _, err := f.tickets.SetPriority(ctx, "human:alex", f.ref.String(), "P0"); err != nil {
		t.Fatal(err)
	}
	res, err := f.plans.Check(ctx, f.run, splitPlan(tickets.PlanTicket{Key: "a", Title: "Half A", Body: childBody, Type: "bug", Size: "S"}), runner.PlanSettings{MaxSize: "L"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	children, err := f.plans.Split(ctx, f.run, *res.Split)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	ref, _ := tickets.ParseRef(children[0])
	created := f.gh.Issue(ref.Number).Body
	table := "| Type | Size | Priority | Repository |\n|---|---|---|---|\n| bug | S | P0 | `" + githubtest.Repo + "` |\n"
	if !strings.HasSuffix(created, table) {
		t.Fatalf("the child's body:\n%s\nwant it to end with the table:\n%s", created, table)
	}
	checker := standard.Checker{Standard: standard.Default(), Root: f.run.Dir, Repo: githubtest.Repo}
	if got, err := checker.Check(ctx, standard.Draft{Body: created}); err != nil || !got.OK() {
		t.Fatalf("the child against the standard: %+v, %v", got.Deviations, err)
	}
}

func TestASplitChildNamingAnotherRepositoryHasItInItsTable(t *testing.T) {
	f := newSplit(t)
	res, err := f.plans.Check(ctx, f.run, splitPlan(tickets.PlanTicket{Key: "a", Title: "Half A", Body: childBody, Type: "feature", Size: "M", Repo: "acme/lib"}), runner.PlanSettings{MaxSize: "L"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if body := res.Split.Tickets[0].Body; !strings.HasSuffix(body, "| feature | M | P2 | `acme/lib` |\n") {
		t.Fatalf("the child's body:\n%s\nwant acme/lib and the default priority in its table", body)
	}
}

func TestASplitWithAChildThatMissesTheStandardIsRefusedBeforeAnyIssue(t *testing.T) {
	for name, tc := range map[string]struct {
		child tickets.PlanTicket
		named string
	}{
		"no size":      {tickets.PlanTicket{Key: "b", Title: "Half B", Body: childBody, Type: "bug"}, "size"},
		"missing path": {tickets.PlanTicket{Key: "b", Title: "Half B", Body: strings.Replace(childBody, "cache/cache.go", "checkpoint_test.go", 1), Type: "bug", Size: "S"}, "checkpoint_test.go"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSplit(t)
			issues := f.gh.IssueCount()
			good := tickets.PlanTicket{Key: "a", Title: "Half A", Body: childBody, Type: "bug", Size: "S"}
			_, err := f.plans.Check(ctx, f.run, splitPlan(good, tc.child), runner.PlanSettings{MaxSize: "L"})
			if err == nil || !strings.Contains(err.Error(), "child b ") || !strings.Contains(err.Error(), tc.named) || strings.Contains(err.Error(), "child a ") {
				t.Fatalf("Check = %v, want child b refused with its %s named", err, tc.named)
			}
			if f.gh.IssueCount() != issues {
				t.Fatalf("issues: %d, want %d: nothing created", f.gh.IssueCount(), issues)
			}
		})
	}
}

func TestASplitMakesAdmittedChildrenAndLeavesTheParentWithoutTasks(t *testing.T) {
	f := newSplit(t)
	gh, parent, ref, k, tasks, plans, run := f.gh, f.parent, f.ref, f.tickets, f.tasks, f.plans, f.run
	impl, _ := tasks.Create(ctx, "system", core.NewTask{Ticket: ref.String(), Project: "web", Role: "implementer", Needs: []string{run.Task}})
	children, err := plans.Split(ctx, run, tickets.Plan{Tickets: []tickets.PlanTicket{{Key: "a", Title: "Half A"}, {Key: "b", Title: "Half B", Needs: []string{"a"}}}})
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(children) != 2 || len(gh.SubIssues(parent)) != 2 {
		t.Fatalf("children = %v, sub-issues %v", children, gh.SubIssues(parent))
	}
	for _, c := range children {
		tk, _ := k.Get(ctx, c)
		if tk.State != core.TicketNew && tk.State != core.TicketReady || tk.BudgetOf != ref.String() {
			t.Fatalf("child %s = %s, budget of %q; want it inheriting the parent's", c, tk.State, tk.BudgetOf)
		}
	}
	p, _ := k.Get(ctx, ref.String())
	if p.State != core.TicketInProgress || len(p.Children) != 2 {
		t.Fatalf("the parent = %s, children %v; want in_progress (T5) with both", p.State, p.Children)
	}
	if got, _ := tasks.Get(ctx, impl.ID); got.State != core.TaskCanceled {
		t.Fatalf("the parent's implement task is %s, want canceled: a split parent has no tasks of its own", got.State)
	}
}
