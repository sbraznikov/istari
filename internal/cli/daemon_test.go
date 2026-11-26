//go:build !istaritest

package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/cli"
)

func TestADaemonNotBuiltForTestsRefusesSteppedLoops(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	cfg := filepath.Join(t.TempDir(), "config.toml")
	code := cli.Execute([]string{"daemon", "--stepped", "--config", cfg}, &stdout, &stderr)
	if code != cli.ExitUsage {
		t.Fatalf("istari daemon --stepped: exit code = %d, want %d (usage); stderr:\n%s", code, cli.ExitUsage, stderr.String())
	}
	want := "--stepped is only for tests: this istari binary was not built for tests"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to say %q", stderr.String(), want)
	}
}
