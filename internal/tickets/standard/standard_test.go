package standard_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/tickets/standard"
)

var ctx = context.Background()

// body builds a ticket body in this repository's format; parts override or
// drop ("-") sections and set the Needs line and the metadata row.
func body(parts map[string]string) string {
	sections := []struct{ name, text string }{
		{"Context", "Rate limits protect the API."},
		{"What needs to be done", "- Add a limiter in `internal/store/store.go`.\n- Add `internal/limits/limits.go` (new)."},
		{"Acceptance criteria", "- [ ] A burst over the limit is refused."},
		{"Out of scope", "- Per-user limits."},
		{"Dependencies", "- Needs: " + or(parts["needs"], "none")},
	}
	var b strings.Builder
	for _, s := range sections {
		text := s.text
		if v, ok := parts[s.name]; ok {
			if v == "-" {
				continue
			}
			text = v
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", s.name, text)
	}
	meta := or(parts["meta"], "| feature | M | P1 | `acme/web` | `docs/design.md`, section 4 |")
	b.WriteString("---\n\n| Type | Size | Priority | Repository | Spec |\n|---|---|---|---|---|\n" + meta + "\n")
	return b.String()
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

type fixture struct {
	gh      *githubtest.Server
	checker standard.Checker
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gh := githubtest.New(t)
	root := t.TempDir()
	for _, f := range []string{"internal/store/store.go", "docs/design.md", "go.mod", "Makefile", ".gitignore"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{gh: gh, checker: standard.Checker{
		Standard:        standard.Default(),
		Tracker:         github.New(github.Options{BaseURL: gh.URL, Token: gh.Token}),
		Root:            root,
		Repo:            githubtest.Repo,
		DefaultPriority: "P2",
	}}
}

func (f *fixture) check(t *testing.T, d standard.Draft) standard.Result {
	t.Helper()
	res, err := f.checker.Check(ctx, d)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

func rules(res standard.Result) []string {
	var out []string
	for _, d := range res.Deviations {
		out = append(out, d.Rule+": "+d.Message)
	}
	return out
}

func assertDeviation(t *testing.T, res standard.Result, rule, says string) {
	t.Helper()
	for _, d := range res.Deviations {
		if d.Rule == rule && strings.Contains(d.Message, says) {
			return
		}
	}
	t.Fatalf("deviations %q lack a %s deviation mentioning %q", rules(res), rule, says)
}

func TestAConformingTicketHasNoDeviations(t *testing.T) {
	f := newFixture(t)
	res := f.check(t, standard.Draft{Body: body(nil)})
	if len(res.Deviations) != 0 || res.Type != "feature" || res.Size != "M" || res.Priority != "P1" || res.Repository != "acme/web" {
		t.Fatalf("result = %+v, deviations %q", res, rules(res))
	}
}

func TestAMissingOrEmptySectionIsNamed(t *testing.T) {
	f := newFixture(t)
	assertDeviation(t, f.check(t, standard.Draft{Body: body(map[string]string{"Out of scope": "-"})}), "section", "Out of scope")
	assertDeviation(t, f.check(t, standard.Draft{Body: body(map[string]string{"Acceptance criteria": "  "})}), "section", "Acceptance criteria")
}

func TestAnInvalidSizePriorityOrTypeFails(t *testing.T) {
	f := newFixture(t)
	assertDeviation(t, f.check(t, standard.Draft{Body: body(map[string]string{"meta": "| feature | XXL | P1 | `acme/web` | |"})}), "size", "XXL")
	assertDeviation(t, f.check(t, standard.Draft{Body: body(map[string]string{"meta": "| feature | M | P7 | `acme/web` | |"})}), "priority", "P7")
	assertDeviation(t, f.check(t, standard.Draft{Body: body(map[string]string{"meta": "| wish | M | P1 | `acme/web` | |"})}), "type", "wish")
	// Labels are the tracker's truth; a table that disagrees is a deviation.
	res := f.check(t, standard.Draft{Body: body(nil), Labels: []string{"size:S"}})
	assertDeviation(t, res, "size", "size:S")
}

func TestAMissingPriorityIsDefaultedNotFailed(t *testing.T) {
	f := newFixture(t)
	res := f.check(t, standard.Draft{Body: body(map[string]string{"meta": "| feature | M |  | `acme/web` | |"})})
	if len(res.Deviations) != 0 || res.Priority != "P2" || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "P2") {
		t.Fatalf("result = %+v, deviations %q", res, rules(res))
	}
}

func TestTheRepositoryIsRequiredAndMustMatch(t *testing.T) {
	f := newFixture(t)
	assertDeviation(t, f.check(t, standard.Draft{Body: body(map[string]string{"meta": "| feature | M | P1 |  | |"})}), "repository", "missing")
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: 9}
	assertDeviation(t, f.check(t, standard.Draft{Ref: &ref, Body: body(map[string]string{"meta": "| feature | M | P1 | `acme/other` | |"})}), "repository", "acme/other")
}

func TestDependencyRefsMustExistAndNotBeCanceled(t *testing.T) {
	f := newFixture(t)
	open := f.gh.Seed("dep", body(nil), "alex")
	canceled := f.gh.Seed("dropped", body(nil), "alex")
	f.gh.HumanClose(canceled, "alex", "not_planned")
	res := f.check(t, standard.Draft{Body: body(map[string]string{"needs": fmt.Sprintf("#%d, gh:acme/web#%d, #404", open, canceled)})})
	assertDeviation(t, res, "dependency", "#404")
	assertDeviation(t, res, "dependency", fmt.Sprintf("#%d", canceled))
	if len(res.Deviations) != 2 {
		t.Fatalf("deviations = %q, want only the missing and the canceled dependency", rules(res))
	}
}

func TestACycleInDependenciesFailsAndIsListed(t *testing.T) {
	f := newFixture(t)
	// #1 needs #2, #2 needs #1; the checked ticket is #1.
	f.gh.Seed("a", body(map[string]string{"needs": "#2"}), "alex")
	f.gh.Seed("b", body(map[string]string{"needs": "#1"}), "alex")
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: 1}
	res := f.check(t, standard.Draft{Ref: &ref, Body: body(map[string]string{"needs": "#2"})})
	assertDeviation(t, res, "cycle", "gh:acme/web#1 → gh:acme/web#2 → gh:acme/web#1")
}

func TestBacktickedPathsMustExistUnlessMarkedNew(t *testing.T) {
	f := newFixture(t)
	res := f.check(t, standard.Draft{Body: body(map[string]string{
		"What needs to be done": "- Change `internal/store/missing.go` and `cmd/tool/main.go` (new).\n- Not paths: `istari ticket check`, `--json`, `gh:acme/web#1`, `istari:ready`, `migrations/**`, `T15`.",
	})})
	assertDeviation(t, res, "path", "internal/store/missing.go")
	if len(res.Deviations) != 1 {
		t.Fatalf("deviations = %q, want only the missing path", rules(res))
	}
}

func TestTheStandardComesFromTheRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ticket-standard.toml")
	os.WriteFile(path, []byte("sections = [\"Context\", \"Risks\"]\ntypes = [\"feature\"]\nsizes = [\"S\", \"M\"]\npriorities = [\"P1\", \"P2\"]\nrepository = true\ndependencies = \"Needs\"\n"), 0o644)
	std, err := standard.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.checker.Standard = std
	assertDeviation(t, f.check(t, standard.Draft{Body: body(nil)}), "section", "Risks")
	if _, err := standard.Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
}

func TestOnlyFilePathsAreCheckedAsPaths(t *testing.T) {
	f := newFixture(t)
	notPaths := "`standard.Default`, `cfg.Defaults.Priority`, `os.Stat`, `api.github.com`, `github.com/acme/web`, " +
		"`main...HEAD`, `acme/other`, `v1.2.3`, `~/.config/istari/config.toml`, `../..`, `component#57`"
	res := f.check(t, standard.Draft{Body: body(map[string]string{"Context": "Mentions " + notPaths + "."})})
	if len(res.Deviations) != 0 {
		t.Fatalf("deviations for things that are not paths: %q", rules(res))
	}
	res = f.check(t, standard.Draft{Body: body(map[string]string{"Context": "Edit `Makefile`, `.gitignore`, `go.mod:12`, `README` and `internal/gone/`."})})
	assertDeviation(t, res, "path", "README")
	assertDeviation(t, res, "path", "internal/gone")
	if len(res.Deviations) != 2 {
		t.Fatalf("deviations = %q, want README and internal/gone only", rules(res))
	}
}

func TestCodeFencesAndRulesInsideSectionsDoNotBreakParsing(t *testing.T) {
	f := newFixture(t)
	ctxText := "Example:\n\n```markdown\n## Out of scope\n\n`not/a/real/path.go`\n- Needs: #404\n```\n\n---\n\nMore context after a rule."
	res := f.check(t, standard.Draft{Body: body(map[string]string{"Context": ctxText})})
	if len(res.Deviations) != 0 {
		t.Fatalf("deviations = %q, want none: the fence and the rule are content", rules(res))
	}
}

func TestLabelsAndTablesAreReadIgnoringCase(t *testing.T) {
	f := newFixture(t)
	res := f.check(t, standard.Draft{Body: body(map[string]string{"meta": "| Feature | m | p1 | `acme/web` | |"}), Labels: []string{"Size:M", "Type:feature"}})
	if len(res.Deviations) != 0 || res.Size != "M" || res.Type != "feature" || res.Priority != "P1" {
		t.Fatalf("result = %+v, deviations %q", res, rules(res))
	}
}

func TestCrossRepositoryShorthandsAreResolved(t *testing.T) {
	f := newFixture(t)
	f.gh.Seed("a", body(nil), "alex")
	res := f.check(t, standard.Draft{Body: body(map[string]string{"needs": "#1, component#57, acme/other#3"})})
	want := []string{"gh:acme/web#1", "gh:acme/component#57", "gh:acme/other#3"}
	if strings.Join(res.Needs, " ") != strings.Join(want, " ") {
		t.Fatalf("needs = %v, want %v", res.Needs, want)
	}
}

func TestAShorthandWithoutAKnownRepositoryIsReported(t *testing.T) {
	f := newFixture(t)
	f.checker.Repo = ""
	res := f.check(t, standard.Draft{Body: body(map[string]string{"needs": "#5"})})
	assertDeviation(t, res, "dependency", "#5")
}

func TestADefaultPriorityOutsideTheStandardIsADeviation(t *testing.T) {
	f := newFixture(t)
	f.checker.Standard.Priorities = []string{"P0", "P1"}
	res := f.check(t, standard.Draft{Body: body(map[string]string{"meta": "| feature | M |  | `acme/web` | |"})})
	assertDeviation(t, res, "priority", "P2")
}

func TestTheWorkStateBlockIsNotPartOfTheTicketText(t *testing.T) {
	f := newFixture(t)
	plain := body(nil)
	tracked := journal.WithWorkState(plain, journal.WorkState{Stage: "ready", Branch: "istari/1/x", Next: "Write the test", OpenQuestions: []string{"Which API?"}})
	if standard.Hash(standard.Default(), tracked) != standard.Hash(standard.Default(), plain) {
		t.Fatal("the work state block changes the hash of the required sections: every journal entry would be T15")
	}
	res := f.check(t, standard.Draft{Body: tracked})
	if len(res.Deviations) != 0 || res.Type != "feature" || res.Size != "M" || res.Priority != "P1" {
		t.Fatalf("with the block: %+v, deviations %q", res, rules(res))
	}
	// The block changing (a new entry) does not change the hash either.
	again := journal.WithWorkState(tracked, journal.WorkState{Stage: "planning"})
	if standard.Hash(standard.Default(), again) != standard.Hash(standard.Default(), plain) {
		t.Fatal("a new work state changes the hash")
	}
}
