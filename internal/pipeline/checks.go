package pipeline

import (
	"slices"
	"strings"
)

// When a check runs (spec section 12, at).
const (
	AtCheckpoint = "checkpoint" // at every istari checkpoint
	AtPR         = "pr"         // before istari pr open
)

// Check is one named check of [checks]: a command code runs through sh -c,
// and when.
type Check struct {
	Name string
	Run  string
	At   []string // AtCheckpoint, AtPR
}

// RunsAt tells whether the check runs at a moment.
func (c Check) RunsAt(moment string) bool { return slices.Contains(c.At, moment) }

// Checks are the [checks] of a pipeline (spec sections 7.7 and 12).
type Checks struct {
	List      []Check  // typecheck, test and full first, then the rest by name
	TestFiles []string // glob patterns of test files; empty: the usual conventions
	VerifyRed bool     // a new test must fail on the previous checkpoint's code; true by default
}

// Get returns the check of a name.
func (c Checks) Get(name string) (Check, bool) {
	i := slices.IndexFunc(c.List, func(x Check) bool { return x.Name == name })
	if i < 0 {
		return Check{}, false
	}
	return c.List[i], true
}

// builtin are the checks code knows by name, in order, with when they run
// when the file does not say.
var builtin = []Check{
	{Name: "typecheck", At: []string{AtCheckpoint}},
	{Name: "test", At: []string{AtCheckpoint}},
	{Name: "full", At: []string{AtPR}},
}

// parseChecks reads a [checks] table.
func parseChecks(raw map[string]any) (Checks, error) {
	out := Checks{VerifyRed: true}
	var named []Check
	for name, v := range raw {
		switch name {
		case "verify_red":
			out.VerifyRed, _ = v.(bool)
			continue
		case "test_files":
			list, _ := v.([]any)
			for _, p := range list {
				if s, ok := p.(string); ok {
					out.TestFiles = append(out.TestFiles, s)
				}
			}
			continue
		}
		c := Check{Name: name, At: []string{AtCheckpoint, AtPR}}
		if i := slices.IndexFunc(builtin, func(b Check) bool { return b.Name == name }); i >= 0 {
			c.At = builtin[i].At
		}
		switch val := v.(type) {
		case string:
			c.Run = val
		case map[string]any:
			c.Run, _ = val["run"].(string)
			if at, ok := val["at"].([]any); ok {
				c.At = nil
				for _, a := range at {
					s, _ := a.(string)
					c.At = append(c.At, s)
				}
			}
		}
		named = append(named, c)
	}
	rank := func(c Check) int {
		if i := slices.IndexFunc(builtin, func(b Check) bool { return b.Name == c.Name }); i >= 0 {
			return i
		}
		return len(builtin)
	}
	slices.SortFunc(named, func(a, b Check) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a.Name, b.Name)
	})
	out.List = named
	return out, nil
}
