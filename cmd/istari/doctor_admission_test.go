package main_test

import (
	"os"
	"strings"
	"testing"
)

func TestDoctorShowsTheAdmissionModeOfEachProject(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	bin := t.TempDir()
	if err := os.WriteFile(bin+"/claude", []byte("#!/bin/sh\necho 1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := writeDaemonConfig(t, newDataDir(t), "\n[[project]]\nname = \"web\"\nadmission = \"label\"\n\n[[project]]\nname = \"api\"\n")
	out, stderr, _ := istariEnv(t, repo, config, []string{"PATH=" + bin + ":/usr/bin:/bin"}, "doctor")
	for project, mode := range map[string]string{"web": "label", "api": "trusted"} {
		row := ""
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "admission "+project+" ") {
				row = line
			}
		}
		if !strings.Contains(row, mode) {
			t.Errorf("admission row of %s = %q, want %s\n%s%s", project, row, mode, out, stderr)
		}
	}
}
