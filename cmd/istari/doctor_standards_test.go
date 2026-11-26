package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/exectest"
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
	exectest.Write(t, filepath.Join(bin, "claude"), "#!/bin/sh\necho 1.0.0\n")
	config := writeDaemonConfig(t, newDataDir(t))
	out, stderr, code := istariEnv(t, repo, config, []string{"PATH=" + bin + ":/usr/bin:/bin"}, "doctor")
	return out + stderr, code
}

func TestDoctorWarnsAboutMissingStandardsFiles(t *testing.T) {
	t.Parallel()
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

// writeFiles creates each file, with its parent directories, and each
// directory ending in "/" under root.
func writeFiles(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("# "+p+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorShowsTheStandardsOkWhenTheRepositoryHasThemAll(t *testing.T) {
	t.Parallel()
	for name, adr := range map[string]string{"docs/adr": "docs/adr/", "adr at the root": "adr/"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := t.TempDir()
			git(t, repo, "init", "-q", "-b", "main")
			writeFiles(t, repo, "CODING_STANDARDS.md", "CONTRIBUTING.md", "GLOSSARY.md", adr)
			out, code := doctorInRepo(t, repo)
			row := standardsRow(t, out)
			if code != 0 || !strings.Contains(row, " ok ") || strings.Contains(row, "missing") {
				t.Fatalf("doctor with all standards files: exit %d, row %q, want 0 and ok\n%s", code, row, out)
			}
		})
	}
}

func TestDoctorListsOnlyTheMissingStandardsFiles(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	// A file named adr is not an ADR directory.
	writeFiles(t, repo, "CODING_STANDARDS.md", "GLOSSARY.md", "adr")
	out, _ := doctorInRepo(t, repo)
	row := standardsRow(t, out)
	if !strings.Contains(row, "missing: CONTRIBUTING.md, adr/ or docs/adr/") || strings.Contains(row, "GLOSSARY") || strings.Contains(row, "CODING_STANDARDS") {
		t.Fatalf("standards row = %q, want only CONTRIBUTING.md and the ADR directory missing", row)
	}
}

func TestDoctorOutsideARepositoryShowsNeitherPipelineNorStandards(t *testing.T) {
	t.Parallel()
	out, _ := doctorInRepo(t, t.TempDir())
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "pipeline") || strings.HasPrefix(line, "standards") {
			t.Errorf("doctor outside a repository shows %q\n%s", line, out)
		}
	}
}
