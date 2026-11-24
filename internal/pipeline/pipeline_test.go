package pipeline_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/pipeline"
)

type step struct {
	Stage, Role string
	Needs       []string
}

func steps(specs []pipeline.TaskSpec) []step {
	var out []step
	for _, s := range specs {
		out = append(out, step{s.Stage, s.Role, s.Needs})
	}
	return out
}

func TestTheDefaultPipelineHasTheSevenStagesOfTheSpec(t *testing.T) {
	p := pipeline.Default()
	var names []string
	for _, s := range p.Stages {
		names = append(names, s.Name)
	}
	if want := []string{"refine", "plan", "implement", "verify", "review", "approve", "merge"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("stages = %v, want %v", names, want)
	}
	// Work starts at T3: refine is done by then (the ticket was admitted).
	got := steps(p.Expand(pipeline.Settings{}))
	want := []step{
		{"plan", "planner", nil},
		{"implement", "implementer", []string{"plan"}},
		{"verify", "verify", []string{"implement"}},
		{"review", "reviewer", []string{"implement", "verify"}},
		{"approve", pipeline.RoleHuman, []string{"review"}},
		{"merge", "merge", []string{"approve"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tasks =\n%v\nwant\n%v", got, want)
	}
}

func TestRemovingAStageFromTheFileRemovesItsTask(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
[[stage]]
name = "plan"
role = "planner"

[[stage]]
name = "implement"
role = "implementer"
needs = ["plan"]

[[stage]]
name = "review"
role = "reviewer"
needs = ["implement", "verify"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range steps(p.Expand(pipeline.Settings{})) {
		if s.Stage == "approve" {
			t.Fatalf("the approve stage was removed from the file but has a task: %v", s)
		}
	}
	// verify and merge are code's own stages: they stay.
	if got := steps(p.Expand(pipeline.Settings{})); len(got) != 5 || got[2].Stage != "verify" || got[4].Stage != "merge" || !reflect.DeepEqual(got[4].Needs, []string{"review"}) {
		t.Fatalf("tasks = %v", got)
	}
}

func TestAnUnknownRoleOrACycleIsRejectedWithItsLine(t *testing.T) {
	for _, tc := range []struct{ file, says string }{
		{"[[stage]]\nname = \"plan\"\nrole = \"planner\"\n\n[[stage]]\nname = \"x\"\nrole = \"wizard\"\n", "line 5"},
		{"[[stage]]\nname = \"a\"\nrole = \"planner\"\nneeds = [\"b\"]\n\n[[stage]]\nname = \"b\"\nrole = \"implementer\"\nneeds = [\"a\"]\n", "cycle"},
		{"[[stage]]\nname = \"a\"\nrole = \"planner\"\nneeds = [\"nowhere\"]\n", "line 1"},
	} {
		p, err := pipeline.Parse([]byte(tc.file))
		if err == nil {
			err = p.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%q: %v, want an error that says %q", tc.file, err, tc.says)
		}
	}
	if _, err := pipeline.Parse([]byte("[[stage]\nname=")); err == nil || !strings.Contains(err.Error(), "line") {
		t.Errorf("a syntax error: %v, want its line", err)
	}
}

func TestARepositoryValueOverridesTheGlobalDefault(t *testing.T) {
	global := config.Defaults{PlanGate: true, MaxReviewRounds: 3, MaxAttempts: 3}
	if s := pipeline.Default().Settings(global); !s.PlanGate || s.MaxReviewRounds != 3 || s.MaxAttempts != 3 {
		t.Fatalf("without a file: %+v", s)
	}
	p, err := pipeline.Parse([]byte("[limits]\nmax_review_rounds = 5\n\n[[stage]]\nname = \"plan\"\nrole = \"planner\"\ngate = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s := p.Settings(global); s.PlanGate || s.MaxReviewRounds != 5 || s.MaxAttempts != 3 {
		t.Fatalf("with the file: %+v, want the gate off, 5 rounds, 3 attempts", s)
	}
}

func TestWithThePlanGateTheImplementerWaitsForAHuman(t *testing.T) {
	got := steps(pipeline.Default().Expand(pipeline.Settings{PlanGate: true}))
	if !reflect.DeepEqual(got[1], step{"plan-approval", pipeline.RoleHuman, []string{"plan"}}) {
		t.Fatalf("second task = %v, want the plan approval", got[1])
	}
	if !reflect.DeepEqual(got[2], step{"implement", "implementer", []string{"plan-approval"}}) {
		t.Fatalf("implement = %v, want it to need the approval", got[2])
	}
}
