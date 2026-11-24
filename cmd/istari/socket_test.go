package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestADataDirTooLongForTheSocketIsSaidPlainly(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), strings.Repeat("a-rather-long-directory-name/", 5))
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("[daemon]\ndata_dir = %q\n", dataDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := run(t, "--config", config, "daemon")
	if code == 0 || !strings.Contains(stderr, "shorter data_dir") || !strings.Contains(stderr, "istari.sock") {
		t.Fatalf("daemon with a long data_dir: exit %d %q", code, stderr)
	}
	out, _, code := run(t, "--config", config, "doctor")
	if code != 1 || !strings.Contains(out, "daemon socket") || !strings.Contains(out, "shorter data_dir") {
		t.Fatalf("doctor with a long data_dir: exit %d\n%s", code, out)
	}
	short := filepath.Join(os.TempDir(), "istari-sock-ok")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("[daemon]\ndata_dir = %q\n", short)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := run(t, "--config", config, "doctor"); !strings.Contains(out, "daemon socket") || strings.Contains(out, "shorter data_dir") {
		t.Fatalf("doctor with a short data_dir:\n%s", out)
	}
}

func TestDoctorFailsVCSWhenNeitherAnAppNorGhGivesAToken(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = \"acme/web\"\n", filepath.Join(os.TempDir(), "istari-doc"))
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	noGH := t.TempDir() // a PATH without gh
	out, _, code := istariEnv(t, t.TempDir(), config, []string{"PATH=" + noGH}, "doctor")
	if code != 1 || !rowFails(out, "vcs") {
		t.Fatalf("doctor without gh: exit %d\n%s", code, out)
	}
	out, _, _ = istariEnv(t, t.TempDir(), config, []string{"PATH=" + fakeGH(t, "tok")}, "doctor")
	if rowFails(out, "vcs") {
		t.Fatalf("doctor with gh logged in fails vcs:\n%s", out)
	}
}

// rowFails tells whether the doctor row whose first word is name failed.
func rowFails(doctor, name string) bool {
	for _, line := range strings.Split(doctor, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == name {
			return f[2] == "FAIL"
		}
	}
	return false
}
