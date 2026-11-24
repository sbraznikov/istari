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

func TestAZonePathThatDoesNotExistAndIsNotNewIsRejected(t *testing.T) {
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
	f := newPlans(t)
	if _, err := f.tickets.SetSize(ctx, "agent:refiner-1", f.run.Ticket, "L"); err != nil {
		t.Fatal(err)
	}
	res, _ := runner.ParsePlanResult([]byte(goodPlan))
	if _, err := f.plans.Check(ctx, f.run, res, runner.PlanSettings{MaxSize: "M"}); err == nil || !strings.Contains(err.Error(), "split") {
		t.Fatalf("Check = %v, want a split required", err)
	}
	split, _ := runner.ParsePlanResult([]byte(goodPlan + "split:\n  tickets:\n    - key: one\n      title: First half\n"))
	if _, err := f.plans.Check(ctx, f.run, split, runner.PlanSettings{MaxSize: "M"}); err != nil {
		t.Fatalf("a split plan: %v", err)
	}
}

func TestADirectoryInTheZoneIsRefused(t *testing.T) {
	f := newPlans(t)
	res, _ := runner.ParsePlanResult([]byte(strings.Replace(goodPlan, "cache/cache.go,", "cache/,", 1)))
	if _, err := f.plans.Check(ctx, f.run, res, runner.PlanSettings{MaxSize: "L"}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("Check = %v, want a directory refused: the zone lists files", err)
	}
}

func TestASplitMakesAdmittedChildrenAndLeavesTheParentWithoutTasks(t *testing.T) {
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
	impl, _ := tasks.Create(ctx, "system", core.NewTask{Ticket: ref.String(), Project: "web", Role: "implementer", Needs: []string{plan.ID}})
	plans := runner.NewPlans(nil, journal.New(nil, s, nil), k, nil).WithSplit(tasks, adapter, syncer.Created)
	run := runner.Run{ID: "r_s", Ticket: ref.String(), Task: plan.ID, Role: "planner", Profile: "planner-1"}
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
