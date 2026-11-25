package pipeline

import (
	"fmt"
	"maps"
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

// parseChecks reads the [checks] table raw of a pipeline file, data. What it
// cannot read is refused with its line (CODING_STANDARDS S1.3).
func parseChecks(data []byte, raw map[string]any) (Checks, error) {
	out := Checks{VerifyRed: true}
	var named []Check
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		v := raw[name]
		refuse := func(key, format string, args ...any) error {
			return fmt.Errorf("%s line %d: check %q: %s", Path, checkLine(data, name, key), name, fmt.Sprintf(format, args...))
		}
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
			for _, key := range slices.Sorted(maps.Keys(val)) {
				if key != "run" && key != "at" {
					return Checks{}, refuse(key, "unknown key %q; a check table takes run and at", key)
				}
			}
			var ok bool
			if c.Run, ok = val["run"].(string); !ok {
				return Checks{}, refuse("", "a check table needs run, a command")
			}
			if at, ok := val["at"]; ok {
				list, ok := at.([]any)
				if !ok {
					return Checks{}, refuse("at", "at is a list of checkpoint and pr")
				}
				c.At = nil
				for _, a := range list {
					if s, _ := a.(string); s != AtCheckpoint && s != AtPR {
						return Checks{}, refuse("at", "at %q is not checkpoint or pr", fmt.Sprint(a))
					}
					c.At = append(c.At, a.(string))
				}
			}
		default:
			return Checks{}, refuse("", "a check is a command or a table { run = \"…\", at = [...] }")
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

// checkLine finds where a check is written in a pipeline file: the line of
// its key in a check table [checks.<name>], else the line that starts the
// check, `<name> = …` in [checks] or the header [checks.<name>]; 0 when it
// is not found.
func checkLine(data []byte, name, key string) int {
	table, start := "", 0
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			table = strings.Trim(strings.ReplaceAll(strings.SplitN(line, "#", 2)[0], " ", ""), "[]")
			if table == "checks."+name && start == 0 {
				start = i + 1
			}
			continue
		}
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		switch {
		case table == "checks" && k == name && start == 0:
			start = i + 1
		case table == "checks."+name && key != "" && k == key:
			return i + 1
		}
	}
	return start
}
