package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// standardsRow returns the doctor output line of the standards check.
func standardsRow(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "standards") {
			return line
		}
	}
	t.Fatalf("doctor shows no standards row:\n%s", out)
	return ""
}

// doctorInRepo runs doctor in repo with a fake claude on PATH, so that no
// other row fails, and returns its output and exit code.
func doctorInRepo(t *testing.T, repo string) (string, int) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho 1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := writeDaemonConfig(t, newDataDir(t))
	out, stderr, code := istariEnv(t, repo, config, []string{"PATH=" + bin + ":/usr/bin:/bin"}, "doctor")
	return out + stderr, code
}

func TestDoctorWarnsAboutMissingStandardsFiles(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	out, code := doctorInRepo(t, repo)
	if code != 0 {
		t.Fatalf("doctor with only a warning: exit %d, want 0\n%s", code, out)
	}
	row := standardsRow(t, out)
	for _, want := range []string{"warn", "CODING_STANDARDS.md", "CONTRIBUTING.md", "GLOSSARY.md", "adr/ or docs/adr/"} {
		if !strings.Contains(row, want) {
			t.Errorf("standards row lacks %q: %s", want, row)
		}
	}
}
