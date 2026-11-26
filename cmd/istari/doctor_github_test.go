package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// githubRow returns the doctor output line of the GitHub requests, or "".
func githubRow(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "github requests") {
			return line
		}
	}
	return ""
}

func TestDoctorShowsTheLastHoursGitHubRequestsByKindAndTheQuotaLeft(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	gh.Seed("Count requests", "## Context\n\nCount.\n", "bob")
	gh.SetRateLimit(4321, time.Now().Add(30*time.Minute))
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	cfg := fmt.Sprintf("[daemon]\ndata_dir = %q\n\n[[project]]\nname = \"web\"\nrepo = %q\n", dataDir, githubtest.Repo)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ghDir, _, _ := strings.Cut(fakeGH(t, gh.Token), string(os.PathListSeparator))
	d := startSteppedDaemon(t, dataDir, config, "PATH="+ghDir+string(os.PathListSeparator)+"/usr/bin:/bin", "GITHUB_API_URL="+gh.URL)

	d.step(t, "sync")
	out, stderr, _ := d.cli(t, "doctor")
	row := githubRow(out + stderr)
	for _, want := range []string{"sync ", "labels ", "4321 of 5000 left"} {
		if !strings.Contains(row, want) {
			t.Errorf("github requests row lacks %q: %s", want, row)
		}
	}
}

func TestDoctorWithoutADaemonShowsNoGitHubRequests(t *testing.T) {
	t.Parallel()
	dataDir := newDataDir(t)
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("[daemon]\ndata_dir = %q\n", dataDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, _ := run(t, "--config", config, "doctor")
	if row := githubRow(out + stderr); row != "" {
		t.Errorf("doctor without a daemon shows %q, want no github requests row", row)
	}
}
