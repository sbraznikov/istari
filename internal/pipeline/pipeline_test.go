package pipeline_test

import (
	"os"
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
	t.Parallel()
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
		{"approve", pipeline.RoleGitHubReview, []string{"review"}},
		{"merge", "merge", []string{"approve"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tasks =\n%v\nwant\n%v", got, want)
	}
}

func TestRemovingAStageFromTheFileRemovesItsTask(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestSelfReviewIsOnUnlessThePipelineTurnsItOff(t *testing.T) {
	t.Parallel()
	if !pipeline.Default().Settings(config.Defaults{}).SelfReview {
		t.Fatal("without a file self_review is off, want on (spec 7.6)")
	}
	p, err := pipeline.Parse([]byte("[limits]\nself_review = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Settings(config.Defaults{}).SelfReview {
		t.Fatal("self_review = false left it on")
	}
}

func TestWithThePlanGateTheImplementerWaitsForAHuman(t *testing.T) {
	t.Parallel()
	got := steps(pipeline.Default().Expand(pipeline.Settings{PlanGate: true}))
	if !reflect.DeepEqual(got[1], step{"plan-approval", pipeline.RoleHuman, []string{"plan"}}) {
		t.Fatalf("second task = %v, want the plan approval", got[1])
	}
	if !reflect.DeepEqual(got[2], step{"implement", "implementer", []string{"plan-approval"}}) {
		t.Fatalf("implement = %v, want it to need the approval", got[2])
	}
}

func TestAProjectNamesItsOwnChecksAndWhenTheyRun(t *testing.T) {
	t.Parallel()
	p, err := pipeline.Parse([]byte(`[checks]
typecheck = "go vet ./..."
test      = "go test -short {dirs}"
full      = "go test ./..."
format    = "test -z \"$(gofmt -l .)\""
tidy      = { run = "go mod tidy -diff", at = ["pr"] }
verify_red = false

[checks.lint]
run = "golangci-lint run"
at  = ["checkpoint"]
`))
	if err != nil {
		t.Fatal(err)
	}
	type check struct {
		Name, Run string
		At        []string
	}
	var got []check
	for _, c := range p.Checks.List {
		got = append(got, check{c.Name, c.Run, c.At})
	}
	want := []check{
		{"typecheck", "go vet ./...", []string{"checkpoint"}},
		{"test", "go test -short {dirs}", []string{"checkpoint"}},
		{"full", "go test ./...", []string{"pr"}},
		{"format", `test -z "$(gofmt -l .)"`, []string{"checkpoint", "pr"}},
		{"lint", "golangci-lint run", []string{"checkpoint"}},
		{"tidy", "go mod tidy -diff", []string{"pr"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checks = %+v\nwant %+v", got, want)
	}
	if p.Checks.VerifyRed {
		t.Error("verify_red = false was not read")
	}
}

func TestThisRepositoryChecksFormatAtEveryCheckpointAndTidyBeforeThePullRequest(t *testing.T) {
	t.Parallel()
	file, err := os.ReadFile("../../.istari/pipeline.toml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, run          string
		atCheckpoint, atPR bool
	}{
		{"format", `test -z "$(gofmt -l .)"`, true, true},
		{"tidy", "go mod tidy -diff", false, true},
	} {
		c, ok := p.Checks.Get(tc.name)
		if !ok {
			t.Errorf("no check %q", tc.name)
			continue
		}
		if c.Run != tc.run {
			t.Errorf("%s runs %q, want %q", tc.name, c.Run, tc.run)
		}
		if c.RunsAt(pipeline.AtCheckpoint) != tc.atCheckpoint || c.RunsAt(pipeline.AtPR) != tc.atPR {
			t.Errorf("%s runs at %v, want checkpoint %v and pr %v", tc.name, c.At, tc.atCheckpoint, tc.atPR)
		}
	}
}

func TestACheckTableWithAnUnknownKeyOrMomentIsRefusedWithItsLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ file, says string }{
		{"[checks]\ntest = \"true\"\ntidy = { run = \"go mod tidy -diff\", when = [\"pr\"] }\n", `.istari/pipeline.toml line 3: check "tidy": unknown key "when"`},
		{"[checks]\ntest = \"true\"\n\ntidy = { run = \"x\", at = [\"nightly\"] }\n", `.istari/pipeline.toml line 4: check "tidy": at "nightly" is not checkpoint or pr`},
		{"[checks]\ntest = \"true\"\n\n[checks.lint]\nrun = \"x\"\nevery = \"day\"\n", `.istari/pipeline.toml line 6: check "lint": unknown key "every"`},
		{"[checks]\nlint = { at = [\"pr\"] }\n", `.istari/pipeline.toml line 2: check "lint": a check table needs run`},
		{"[checks]\nlint = 3\n", `.istari/pipeline.toml line 2: check "lint"`},
		{"[checks]\ntest = \"true\"\nverify_red = \"yes\"\n", `.istari/pipeline.toml line 3: verify_red is true or false`},
		{"[checks]\ntest_files = \"**/*_test.go\"\n", `.istari/pipeline.toml line 2: test_files is a list of glob patterns`},
	} {
		_, err := pipeline.Parse([]byte(tc.file))
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%q: %v, want an error that says %q", tc.file, err, tc.says)
		}
	}
}
