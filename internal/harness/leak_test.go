package harness_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNothingSpecificToOneHarnessLeaksOut checks that no production code
// outside the adapter's own package mentions it. The one allowed exception is
// the blank import in the binary's main package that registers it. Tests may
// mention it, e.g. to fake its binary.
func TestNothingSpecificToOneHarnessLeaksOut(t *testing.T) {
	root := filepath.Join("..", "..")
	adapter := filepath.Join(root, "internal", "harness", "claudecode")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (path == adapter || d.Name() == ".git" || d.Name() == "docs") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(strings.ToLower(line), "claude") {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), `_ "github.com/sbraznikov/istari/internal/harness/claudecode"`) {
				continue
			}
			t.Errorf("%s:%d mentions a specific harness: %s", path, i+1, strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
