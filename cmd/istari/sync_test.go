package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// fakeGH puts a gh on PATH that prints token, as `gh auth token` does.
func fakeGH(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = \"auth token\" ] && echo %s\n", token)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestIstariSyncMirrorsTheTrackersIssues(t *testing.T) {
	gh := githubtest.New(t)
	n := gh.Seed("Add rate limits", "## Context\n…", "bob")
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + fakeGH(t, gh.Token), "GITHUB_API_URL=" + gh.URL}
	d := startDaemonWithConfig(t, dataDir, config, env...)

	var report struct{ Created int }
	d.mustCLI(t, &report, "sync")
	var tk struct{ State, Title, Author string }
	d.mustCLI(t, &tk, "ticket", "show", fmt.Sprintf("gh:%s#%d", githubtest.Repo, n))
	if tk.State != "new" || tk.Title != "Add rate limits" || tk.Author != "bob" {
		t.Fatalf("after istari sync the ticket is %+v (report %+v)", tk, report)
	}
	if labels := gh.Issue(n).Labels; len(labels) != 1 || labels[0] != "istari:new" {
		t.Fatalf("issue labels = %v, want [istari:new]", labels)
	}

	// Only the tracker's line matters here: whether a harness is installed
	// depends on the machine.
	cmd := exec.Command(binary, "--config", config, "doctor")
	cmd.Env = append(os.Environ(), env...)
	out, _ := cmd.CombinedOutput()
	if !trackerOK(string(out)) {
		t.Fatalf("doctor does not report the tracker token as ok:\n%s", out)
	}
}

func trackerOK(doctor string) bool {
	for _, line := range strings.Split(doctor, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "tracker" && fields[1] == "github" {
			return fields[2] == "ok"
		}
	}
	return false
}

func TestIstariSyncWithoutATrackerIsRefused(t *testing.T) {
	d := startDaemon(t, newDataDir(t))
	if _, stderr, code := d.cli(t, "sync"); code != 1 || !strings.Contains(stderr, "sync_disabled") {
		t.Fatalf("istari sync without a tracker: exit %d %q", code, stderr)
	}
}
