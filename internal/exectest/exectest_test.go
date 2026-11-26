package exectest_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sbraznikov/istari/internal/exectest"
)

func TestWriteMakesAScriptThatStarts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fake")

	exectest.Write(t, path, "#!/bin/sh\necho hello\n")

	out, err := exec.Command(path).Output()
	if err != nil {
		t.Fatalf("start %s: %v", path, err)
	}
	if string(out) != "hello\n" {
		t.Errorf("output = %q, want %q", out, "hello\n")
	}
}
