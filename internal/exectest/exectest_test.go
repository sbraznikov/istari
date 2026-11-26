package exectest_test

import (
	"fmt"
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

// On Linux a fork made while another goroutine still holds a script open
// for writing inherits that descriptor, and starting the script then fails
// with "text file busy" (golang/go#22315).
func TestWriteNeverLeavesAScriptBusyForAConcurrentStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 400 {
		name := fmt.Sprintf("fake-%d", i)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, name)
			exectest.Write(t, path, "#!/bin/sh\nexit 0\n")
			if err := exec.Command(path).Run(); err != nil {
				t.Fatalf("start %s: %v", path, err)
			}
		})
	}
}
