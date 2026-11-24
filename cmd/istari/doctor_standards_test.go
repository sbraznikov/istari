package main_test

import (
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

func TestDoctorWarnsAboutMissingStandardsFiles(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	config := writeDaemonConfig(t, newDataDir(t))
	out, stderr, code := istariEnv(t, repo, config, []string{"PATH=/usr/bin:/bin"}, "doctor")
	if code != 0 {
		t.Fatalf("doctor with only a warning: exit %d\n%s%s", code, out, stderr)
	}
	row := standardsRow(t, out)
	for _, want := range []string{"warn", "CODING_STANDARDS.md", "CONTRIBUTING.md", "GLOSSARY.md", "adr/ or docs/adr/"} {
		if !strings.Contains(row, want) {
			t.Errorf("standards row lacks %q: %s", want, row)
		}
	}
}
