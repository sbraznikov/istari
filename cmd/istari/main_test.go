package main_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "istari-bin")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "istari")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("run %v: %v", args, err)
	}
	return out.String(), errOut.String(), code
}

func TestVersionPrintsAVersionAndExitsZero(t *testing.T) {
	out, _, code := run(t, "--version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(out, "istari ") {
		t.Fatalf("stdout = %q, want it to start with %q", out, "istari ")
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"no-such-command"},
		{"--no-such-flag"},
	} {
		_, stderr, code := run(t, args...)
		if code != 2 {
			t.Errorf("istari %v: exit code = %d, want 2", args, code)
		}
		if stderr == "" {
			t.Errorf("istari %v: stderr is empty, want an explanation", args)
		}
	}
}
