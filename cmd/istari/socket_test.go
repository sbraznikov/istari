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
