package skill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/skill"
)

// The rules each built-in instruction must carry, written out from the spec
// (sections 4.3, 4.5, 4.7, 4.9, 7.6, 7.7): rule → phrases that state it.
var roleRules = map[string]map[string][]string{
	"refiner": {
		"meaning unchanged, no new requirements": {"Do not change the meaning of the ticket", "do not add requirements the author did not ask for"},
		"inferences are assumptions":             {"Everything you inferred yourself is an assumption", "istari assume"},
		"original kept verbatim, changes listed": {"keep the original text verbatim in a collapsed block", "list in a comment what you changed and which assumptions you made"},
		"ask and needs_input":                    {"istari ask", "needs_input"},
		"size set, author's checked":             {"Set the T-shirt size", "When the author gave a size, check it", "correct it and explain why"},
		"duplicates become a decision card":      {"candidates code selected", "Do not resolve a duplicate or a contradiction silently", "decision card"},
		"checked against its documents":          {"check the ticket against them", "links the documents its behaviour is defined in", "changes documented behaviour", "deviation"},
	},
	"planner": {
		"structured plan":            {"istari plan submit", "slices", "seams", "change zone"},
		"seams agreed in advance":    {"Seams agreed in advance", "what it catches and what it misses"},
		"zone paths exist or new":    {"exists in the repository or is marked as new"},
		"may revise the size":        {"You may revise the ticket's size"},
		"split above max_size (T5)":  {"larger than `max_size`", "must be split into child tickets (T5)"},
		"slices name their sections": {"check the ticket against them", "names the sections of the documents it follows"},
	},
	"implementer": {
		"order of work":           {"Read the ticket, the plan and the journal", "red → green", "full test suite once, at the end", "istari checkpoint", "istari pr open"},
		"seams agreed in advance": {"Seams are agreed in advance"},
		"vertical slices":         {"Vertical slices", "Writing all the tests first and then all the code is forbidden"},
		"red first":               {"Red first"},
		"refactoring outside":     {"Refactoring is not part of the cycle"},
		"forbidden tests":         {"Implementation-coupled tests", "tautological tests"},
		"project language":        {"the project's glossary", "ADRs"},
		"PR description":          {"**Summary**", "**Evidence**", "**Merge Danger**", "one-way or a two-way door", "blast radius"},
		"self-review, two axes":   {"review your own work along two axes", "Standards and Spec"},
		"answering a smell":       {"either fix the code or reply why it is not a flaw here with `istari review reply --finding <n>"},
		"a larger size":           {"the size turned out larger"},
		"brief PR prose":          {"keep the prose brief", "domain language from its glossary"},
		"ADRs are replaced":       {"replaced by a new ADR"},
	},
	"reviewer": {
		"two axes":                   {"**Standards**", "**Spec**", "Quote the line of the ticket"},
		"independent passes":         {"two independent passes with a clean context"},
		"not merged or reranked":     {"Report them separately", "Do not merge or rerank findings across the axes"},
		"smell baseline":             {"mysterious name", "duplicated code", "feature envy", "data clumps", "primitive obsession", "repeated switches", "shotgun surgery", "divergent change", "speculative generality", "message chains", "middle man", "refused bequest"},
		"repository standard wins":   {"A documented repository standard is stronger than the baseline"},
		"smells are judgement calls": {"judgement call"},
		"one line per axis":          {"one line per axis"},
		"structured report":          {"istari review submit"},
		"hard vs judgement":          {"Distinguish hard violations of a documented standard from judgement calls"},
		"smells defined":             {"a method that reaches into another object's data more than its own", "the same few fields or parameters keep travelling together"},
		"next round":                 {"In the next round", "accept the explanation or insist"},
		"conflict resolution only":   {"check only the conflict resolution"},
		"spec against documents":     {"against the documents the ticket links to", "changes documented behaviour the ticket does not ask for", "contradicts an ADR", "any file of the repository"},
	},
}

// instruction is a role's built-in instruction with runs of white space as
// one space, as Markdown wraps a sentence across lines.
func instruction(t *testing.T, role string) string {
	t.Helper()
	text, err := skill.Instruction(role, nil)
	if err != nil {
		t.Fatalf("%s: %v", role, err)
	}
	return strings.Join(strings.Fields(text), " ")
}

// The rules every role carries (spec section 7.7): it works by the sources of
// truth its repository's instructions file names, and stops on a document
// that is wrong or in conflict.
var everyRoleRules = map[string][]string{
	"sources of truth":      {"sources of truth", "instructions file", "the documents the ticket links to"},
	"a document is wrong":   {"wrong or in conflict", "istari ask", "needs_input", "Do not implement either side silently"},
	"a document-fix ticket": {"a ticket of its own", "section 4.5", "this ticket needs it"},
}

// repositoryDocuments are the sources of truth this repository's AGENTS.md
// lists, and the standards files istari looks for: a role names none of them.
var repositoryDocuments = []string{"docs/design.md", "docs/tui", "CODING_STANDARDS.md", "CONTRIBUTING.md", "GLOSSARY.md"}

func TestEveryRoleWorksByTheRepositorysSourcesOfTruth(t *testing.T) {
	t.Parallel()
	for role := range roleRules {
		text := instruction(t, role)
		for rule, phrases := range everyRoleRules {
			for _, phrase := range phrases {
				if !strings.Contains(text, phrase) {
					t.Errorf("%s: rule %q: the instruction lacks %q", role, rule, phrase)
				}
			}
		}
		for _, doc := range repositoryDocuments {
			if strings.Contains(text, doc) {
				t.Errorf("%s: the instruction names %s, a document of this repository", role, doc)
			}
		}
	}
}

func TestTheBuiltInInstructionsCarryTheRulesOfTheSpec(t *testing.T) {
	t.Parallel()
	for role, rules := range roleRules {
		text := instruction(t, role)
		for rule, phrases := range rules {
			for _, phrase := range phrases {
				if !strings.Contains(text, phrase) {
					t.Errorf("%s: rule %q: the instruction lacks %q", role, rule, phrase)
				}
			}
		}
	}
}

func TestARoleFileInTheRepositoryReplacesTheBuiltInOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".istari", "roles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".istari", "roles", "planner.md"), []byte("# Our planner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, path)) }
	if text, err := skill.Instruction("planner", read); err != nil || text != "# Our planner\n" {
		t.Fatalf("planner = %q, %v; want the repository's file", text, err)
	}
	if text, _ := skill.Instruction("reviewer", read); !strings.Contains(text, "two independent passes") {
		t.Fatal("a role without a file of its own lost its built-in instruction")
	}
	if _, err := skill.Instruction("wizard", read); err == nil {
		t.Fatal("an unknown role has an instruction")
	}
}
