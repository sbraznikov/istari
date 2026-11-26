package main_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/exectest"
)

var binary string

func TestMain(m *testing.M) {
	// An agent's run sets ISTARI_SOCKET and ISTARI_TOKEN; inherited, they send
	// the binaries the tests start to the real daemon (#154).
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "ISTARI_") {
			os.Unsetenv(name)
		}
	}
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
	t.Parallel()
	out, _, code := run(t, "--version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(out, "istari ") {
		t.Fatalf("stdout = %q, want it to start with %q", out, "istari ")
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	t.Parallel()
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

func TestDoctorReportsAMissingHarnessBinaryClearly(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	cmd := exec.Command(binary, "doctor")
	cmd.Env = append(os.Environ(), "PATH="+empty)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("doctor without the harness binary: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "not found in PATH") {
		t.Fatalf("doctor output does not explain what is missing:\n%s", out)
	}

	withFake := t.TempDir()
	fake := "#!/bin/sh\necho 1.0.0\n"
	exectest.Write(t, filepath.Join(withFake, "claude"), fake)
	// Only the harness row is judged: other checks (gh, tracker token, user
	// config) depend on the machine and may fail with exit 1.
	cmd = exec.Command(binary, "doctor")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "PATH="+withFake+":/usr/bin:/bin")
	out, err = cmd.CombinedOutput()
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		t.Fatalf("doctor with the harness binary: %v\n%s", err, out)
	}
	if !harnessOK(string(out), "claude-code") {
		t.Fatalf("doctor does not report the harness ok with claude on PATH:\n%s", out)
	}
}

// harnessOK reports whether the doctor output has the row "harness <name> ok".
func harnessOK(doctor, name string) bool {
	for _, line := range strings.Split(doctor, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "harness" && fields[1] == name {
			return fields[2] == "ok"
		}
	}
	return false
}
