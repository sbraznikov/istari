package project_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/project"
)

func TestScaffoldWritesValidTOMLAndKeepsExistingFiles(t *testing.T) {
	t.Parallel()
	for _, lang := range []map[string]string{{"go.mod": "module x\n"}, {}} {
		root := tree(t, lang)
		if err := os.MkdirAll(filepath.Join(root, ".istari"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".istari", "skill.md"), []byte("ours\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		created, err := project.Scaffold(root)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(created, ".istari/skill.md") {
			t.Fatal("Scaffold overwrote an existing file")
		}
		if data, _ := os.ReadFile(filepath.Join(root, ".istari", "skill.md")); string(data) != "ours\n" {
			t.Fatal("an existing file changed")
		}
		var pipeline struct {
			Checks map[string]any
			Merge  struct {
				IrreversiblePaths []string `toml:"irreversible_paths"`
			}
			Stage []struct{ Name, Role string }
		}
		if _, err := toml.DecodeFile(filepath.Join(root, ".istari", "pipeline.toml"), &pipeline); err != nil {
			t.Fatalf("pipeline.toml (%v): %v", lang, err)
		}
		_, isGo := lang["go.mod"]
		if isGo != slices.Contains(pipeline.Merge.IrreversiblePaths, "go.mod") || isGo != (pipeline.Checks["full"] == "go test ./...") {
			t.Fatalf("pipeline for %v = %+v", lang, pipeline)
		}
		var standard map[string]any
		if _, err := toml.DecodeFile(filepath.Join(root, ".istari", "ticket-standard.toml"), &standard); err != nil {
			t.Fatalf("ticket-standard.toml: %v", err)
		}
	}
}

// The commented examples of [checks]: a line "# <Language>:" opens a group,
// each "#   <key> = <value>" below it is one example.
func TestTheInitTemplateShowsCheckExamplesForSeveralLanguages(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{})
	if _, err := project.Scaffold(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".istari", "pipeline.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Parse(data); err != nil {
		t.Fatalf("the written pipeline: %v", err)
	}
	groups := map[string][]string{}
	language := ""
	for _, line := range strings.Split(string(data), "\n") {
		if name, ok := strings.CutPrefix(line, "# "); ok && strings.HasSuffix(name, ":") && !strings.Contains(name, " ") {
			language = strings.TrimSuffix(name, ":")
		} else if example, ok := strings.CutPrefix(line, "#   "); ok && language != "" {
			groups[language] = append(groups[language], example)
		} else {
			language = ""
		}
	}
	for _, want := range []string{"Go", "Python", "TypeScript"} {
		if len(groups[want]) == 0 {
			t.Errorf("no examples for %s in:\n%s", want, data)
		}
	}
	tables, placeholders, testFiles := 0, 0, 0
	for language, examples := range groups {
		file := "[checks]\n" + strings.Join(examples, "\n") + "\n"
		p, err := pipeline.Parse([]byte(file))
		if err != nil {
			t.Errorf("the %s examples do not parse: %v\n%s", language, err, file)
			continue
		}
		for _, c := range p.Checks.List {
			if len(c.At) == 1 {
				tables++
			}
			if c.TakesTests() {
				placeholders++
			}
		}
		testFiles += len(p.Checks.TestFiles)
	}
	if tables == 0 || placeholders == 0 || testFiles == 0 {
		t.Errorf("examples with at %d, with {files}/{dirs} %d, test_files %d: want each shown", tables, placeholders, testFiles)
	}
}

// The pipelines istari init writes, and this repository's own, which was
// made from the same template.
func TestTheInitPipelineAndThisRepositorysLoadAndTheirReviewWaitsForVerify(t *testing.T) {
	t.Parallel()
	files := map[string]string{"this repository": "../../.istari/pipeline.toml"}
	for name, lang := range map[string]map[string]string{"init, Go": {"go.mod": "module x\n"}, "init, no language": {}} {
		root := tree(t, lang)
		if _, err := project.Scaffold(root); err != nil {
			t.Fatal(err)
		}
		files[name] = filepath.Join(root, ".istari", "pipeline.toml")
	}
	for name, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := pipeline.Parse(data)
		if err == nil {
			err = p.Validate()
		}
		if err != nil {
			t.Errorf("%s: the pipeline does not load: %v", name, err)
			continue
		}
		// Warnings names a reviewer stage that does not need verify.
		if !slices.ContainsFunc(p.Stages, func(s pipeline.Stage) bool { return s.Role == "reviewer" }) {
			t.Errorf("%s: the pipeline has no reviewer stage, want one", name)
		}
		if w := p.Warnings(); len(w) != 0 {
			t.Errorf("%s: the pipeline warns %q, want its review to wait for verify", name, w)
		}
	}
}
