// Package pipeline reads a repository's pipeline (.istari/pipeline.toml, spec
// sections 4.3 and 12), expands it into the tasks of a ticket and moves the
// ticket along as they complete.
package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sbraznikov/istari/internal/config"
)

// Path is where a repository keeps its pipeline.
const Path = ".istari/pipeline.toml"

// Stages that code performs itself: every pipeline has them, declared or not.
const (
	StageVerify = "verify" // CI is green
	StageMerge  = "merge"  // code merges
)

// Roles of tasks that no agent runs.
const (
	RoleHuman  = "human"  // a person approves: a gate
	RoleVerify = "verify" // code waits for the checks
	RoleMerge  = "merge"  // code merges
	// RoleGitHubReview is a human approval given as a GitHub review (spec
	// section 4.3): not a gate of istari gate approve.
	RoleGitHubReview = "github_review"
)

// AgentRoles are the roles agents run (spec section 7.1).
var AgentRoles = []string{"refiner", "planner", "implementer", "reviewer"}

// Stage is one stage of a pipeline.
type Stage struct {
	Name      string   `toml:"name"`
	Role      string   `toml:"role"`
	Kind      string   `toml:"kind"` // "" (an agent's), human, verify or merge
	Needs     []string `toml:"-"`
	Harness   string   `toml:"harness"`
	Gate      *bool    `toml:"gate"` // plan stage: overrides plan_gate
	Via       string   `toml:"via"`
	Isolation string   `toml:"isolation"`
	Timeout   string   `toml:"timeout"`
	Standard  string   `toml:"standard"`

	line int // where the stage starts in its file; 0 for built-in ones
}

// Limits are the [limits] of a pipeline; zero means the global default.
type Limits struct {
	MaxAttempts     int    `toml:"max_attempts"`
	MaxReviewRounds int    `toml:"max_review_rounds"`
	MaxSize         string `toml:"max_size"`
	Questions       string `toml:"questions"`
	MaxAssumptions  int    `toml:"max_assumptions"`
	SelfReview      *bool  `toml:"self_review"`
}

// Pipeline is a repository's pipeline.
type Pipeline struct {
	Limits Limits         `toml:"limits"`
	Checks Checks         `toml:"-"`
	Merge  map[string]any `toml:"merge"`
	Stages []Stage        `toml:"stage"`
}

// Default is the pipeline of spec section 4.3, for a repository without a file.
func Default() Pipeline {
	return Pipeline{Stages: []Stage{
		{Name: "refine", Role: "refiner"},
		{Name: "plan", Role: "planner", Needs: []string{"refine"}},
		{Name: "implement", Role: "implementer", Needs: []string{"plan"}},
		{Name: StageVerify, Kind: RoleVerify, Needs: []string{"implement"}},
		{Name: "review", Role: "reviewer", Needs: []string{"implement", StageVerify}},
		{Name: "approve", Kind: RoleHuman, Via: "github_review", Needs: []string{"review"}},
		{Name: StageMerge, Kind: RoleMerge, Needs: []string{"approve"}},
	}}
}

// Parse reads a pipeline file. The stages it lists are the pipeline's, in
// order; the code stages verify and merge are added when it does not list
// them: verify after implement, merge after the last stage.
func Parse(data []byte) (Pipeline, error) {
	// A stage that names no needs follows the stage before it; needs = []
	// runs it in parallel (spec section 4.3: stages can be made parallel).
	var raw struct {
		Checks map[string]any `toml:"checks"`
		Stages []struct {
			Needs *[]string `toml:"needs"`
		} `toml:"stage"`
	}
	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		var perr toml.ParseError
		if errors.As(err, &perr) {
			return Pipeline{}, fmt.Errorf("%s line %d: %s", Path, perr.Position.Line, perr.Message)
		}
		return Pipeline{}, fmt.Errorf("%s: %w", Path, err)
	}
	var p Pipeline
	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&p); err != nil {
		var perr toml.ParseError
		if errors.As(err, &perr) {
			return Pipeline{}, fmt.Errorf("%s line %d: %s", Path, perr.Position.Line, perr.Message)
		}
		return Pipeline{}, fmt.Errorf("%s: %w", Path, err)
	}
	checks, err := parseChecks(data, raw.Checks)
	if err != nil {
		return Pipeline{}, err
	}
	p.Checks = checks
	if len(p.Stages) == 0 { // a file of limits and checks keeps the default stages
		p.Stages = Default().Stages
		return p, nil
	}
	for i := range p.Stages {
		switch {
		case raw.Stages[i].Needs != nil:
			p.Stages[i].Needs = *raw.Stages[i].Needs
		case i > 0:
			p.Stages[i].Needs = []string{p.Stages[i-1].Name}
		}
	}
	headers := 0
	for i, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[[stage]]") && headers < len(p.Stages) {
			p.Stages[headers].line = i + 1
			headers++
		}
	}
	names := func() []string {
		var out []string
		for _, s := range p.Stages {
			out = append(out, s.Name)
		}
		return out
	}
	if !slices.Contains(names(), StageVerify) {
		if i := slices.Index(names(), "implement"); i >= 0 {
			p.Stages = slices.Insert(p.Stages, i+1, Stage{Name: StageVerify, Kind: RoleVerify, Needs: []string{"implement"}})
		}
	}
	if !slices.Contains(names(), StageMerge) && len(p.Stages) > 0 {
		p.Stages = append(p.Stages, Stage{Name: StageMerge, Kind: RoleMerge, Needs: []string{p.Stages[len(p.Stages)-1].Name}})
	}
	return p, nil
}

// at names where a stage is, for an error.
func (s Stage) at() string {
	if s.line == 0 {
		return fmt.Sprintf("stage %q", s.Name)
	}
	return fmt.Sprintf("%s line %d: stage %q", Path, s.line, s.Name)
}

// role is the role of the stage's task.
func (s Stage) role() string {
	switch {
	case s.Kind == RoleHuman && s.Via == "github_review":
		return RoleGitHubReview
	case s.Kind != "":
		return s.Kind
	}
	return s.Role
}

// Validate checks the stages: a name each, once; a known role or kind;
// needs that name stages; no cycle.
func (p Pipeline) Validate() error {
	index := map[string]Stage{}
	for _, s := range p.Stages {
		switch {
		case s.Name == "":
			return fmt.Errorf("%s line %d: a stage needs a name", Path, s.line)
		case index[s.Name].Name != "":
			return fmt.Errorf("%s: a second stage named %q", s.at(), s.Name)
		case s.Kind == "" && !slices.Contains(AgentRoles, s.Role):
			return fmt.Errorf("%s: role %q is not one of %s", s.at(), s.Role, strings.Join(AgentRoles, ", "))
		case s.Kind != "" && !slices.Contains([]string{RoleHuman, RoleVerify, RoleMerge}, s.Kind):
			return fmt.Errorf("%s: kind %q is not human, verify or merge", s.at(), s.Kind)
		}
		index[s.Name] = s
	}
	for _, s := range p.Stages {
		for _, n := range s.Needs {
			if _, ok := index[n]; !ok {
				return fmt.Errorf("%s: needs %q, which is no stage", s.at(), n)
			}
		}
	}
	// A cycle: a stage reached again while its needs are walked.
	const (
		walking = 1
		done    = 2
	)
	mark := map[string]int{}
	var walk func(name string, path []string) error
	walk = func(name string, path []string) error {
		switch mark[name] {
		case walking:
			return fmt.Errorf("%s: a cycle of needs: %s", index[name].at(), strings.Join(append(path, name), " → "))
		case done:
			return nil
		}
		mark[name] = walking
		for _, n := range index[name].Needs {
			if err := walk(n, append(path, name)); err != nil {
				return err
			}
		}
		mark[name] = done
		return nil
	}
	for _, s := range p.Stages {
		if err := walk(s.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

// Settings are the values that decide how a ticket goes through the
// pipeline: the repository's, else the installation's defaults.
type Settings struct {
	MaxSize         string // tickets larger than this are split before T4; "L" by default
	SelfReview      bool   // the implementer reviews its own work before the pull request; true by default
	PlanGate        bool
	MaxReviewRounds int
	MaxAttempts     int
}

// Settings resolves the pipeline's values over the installation's defaults.
func (p Pipeline) Settings(global config.Defaults) Settings {
	s := Settings{MaxSize: "L", SelfReview: p.Limits.SelfReview == nil || *p.Limits.SelfReview, PlanGate: global.PlanGate, MaxReviewRounds: global.MaxReviewRounds, MaxAttempts: global.MaxAttempts}
	if p.Limits.MaxSize != "" {
		s.MaxSize = p.Limits.MaxSize
	}
	if p.Limits.MaxReviewRounds > 0 {
		s.MaxReviewRounds = p.Limits.MaxReviewRounds
	}
	if p.Limits.MaxAttempts > 0 {
		s.MaxAttempts = p.Limits.MaxAttempts
	}
	for _, st := range p.Stages {
		if st.Name == "plan" && st.Gate != nil {
			s.PlanGate = *st.Gate
		}
	}
	return s
}

// TaskSpec is one task a ticket gets when its work starts.
type TaskSpec struct {
	Stage string
	Role  string
	Needs []string // stage names
}

// PlanApproval is the stage of the plan gate: a human approves the plan
// before the implementer starts (spec section 4.3, plan_gate).
const PlanApproval = "plan-approval"

// Expand returns the tasks of a ticket whose work starts (T3), in order.
// Stages before planning (refine) are done by then and are left out, as are
// the needs on them. With the plan gate, a human's task follows the plan and
// whatever needed the plan needs the approval instead.
func (p Pipeline) Expand(s Settings) []TaskSpec {
	skipped := map[string]bool{}
	var out []TaskSpec
	for _, st := range p.Stages {
		if st.role() == "refiner" {
			skipped[st.Name] = true
			continue
		}
		var needs []string
		for _, n := range st.Needs {
			switch {
			case skipped[n]:
			case n == "plan" && s.PlanGate:
				needs = append(needs, PlanApproval)
			default:
				needs = append(needs, n)
			}
		}
		out = append(out, TaskSpec{Stage: st.Name, Role: st.role(), Needs: needs})
		if st.Name == "plan" && s.PlanGate {
			out = append(out, TaskSpec{Stage: PlanApproval, Role: RoleHuman, Needs: []string{"plan"}})
		}
	}
	return out
}
