package project_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/sbraznikov/istari/internal/project"
)

func TestScaffoldWritesValidTOMLAndKeepsExistingFiles(t *testing.T) {
	t.Parallel()
	for _, lang := range []map[string]string{{"go.mod": "module x\n"}, {}} {
		root := tree(t, lang)
		os.MkdirAll(filepath.Join(root, ".istari"), 0o755)
		os.WriteFile(filepath.Join(root, ".istari", "skill.md"), []byte("ours\n"), 0o644)
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
