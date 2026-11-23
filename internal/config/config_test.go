package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/config"
)

func TestSpecExampleLoads(t *testing.T) {
	cfg, err := config.Load("testdata/spec-example.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Daemon.MaxConcurrentRuns; got != 4 {
		t.Errorf("daemon.max_concurrent_runs = %d, want 4", got)
	}
	if got := cfg.Defaults.Merge.AutoMaxSize; got != "S" {
		t.Errorf("defaults.merge.auto_max_size = %q, want S", got)
	}
	if got := cfg.Defaults.BudgetUSD["M"]; got != 15 {
		t.Errorf("defaults.budget_usd.M = %v, want 15", got)
	}
	if got := cfg.Slack.OpsChannel; got != "#agents-ops" {
		t.Errorf("slack.ops_channel = %q, want #agents-ops", got)
	}
	if len(cfg.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(cfg.Projects))
	}
	billing := cfg.Projects[1]
	if billing.Name != "billing" || len(billing.Repos) != 2 {
		t.Fatalf("second project = %+v, want billing with two repos", billing)
	}
	if !billing.Repos[0].Primary || billing.Repos[1].DoneWhen != "released" {
		t.Errorf("billing repos = %+v, want the first primary and the second done_when=released", billing.Repos)
	}
	if got := cfg.Projects[0].Tickets.Provider; got != "github" {
		t.Errorf("web tickets.provider = %q, want github", got)
	}
}

// writeConfig writes body to a temporary config file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertErrorNames(t *testing.T, err error, key string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Load succeeded, want an error naming %q", key)
	}
	if !strings.Contains(err.Error(), key) {
		t.Fatalf("error %q does not name %q", err, key)
	}
}

func TestUnknownKeyIsNamed(t *testing.T) {
	for _, tc := range []struct{ body, key string }{
		{"[daemon]\nmax_concurent_runs = 4\n", "daemon.max_concurent_runs"},
		{"[[project]]\nname = \"web\"\nbranch = \"main\"\n", "project.branch"},
	} {
		_, err := config.Load(writeConfig(t, tc.body))
		assertErrorNames(t, err, tc.key)
	}
}

func TestWrongTypeIsNamed(t *testing.T) {
	for _, tc := range []struct{ body, key string }{
		{"[daemon]\nmax_concurrent_runs = \"four\"\n", "daemon.max_concurrent_runs"},
		{"[defaults]\nplan_gate = \"no\"\n", "defaults.plan_gate"},
	} {
		_, err := config.Load(writeConfig(t, tc.body))
		assertErrorNames(t, err, tc.key)
	}
}

func TestInvalidValueIsNamed(t *testing.T) {
	for _, tc := range []struct{ body, key string }{
		{"[daemon]\nmax_concurrent_runs = 0\n", "daemon.max_concurrent_runs"},
		{"[defaults]\npriority = \"P9\"\n", "defaults.priority"},
		{"[defaults]\nzones = \"sometimes\"\n", "defaults.zones"},
		{"[defaults.merge]\nmode = \"yolo\"\n", "defaults.merge.mode"},
		{"[defaults.merge]\nauto_max_size = \"XXL\"\n", "defaults.merge.auto_max_size"},
		{"[defaults.merge]\nmethod = \"octopus\"\n", "defaults.merge.method"},
		{"[defaults.budget_usd]\nHUGE = 100\n", "defaults.budget_usd.HUGE"},
		{"[[project]]\nrepo = \"acme/web\"\n", "project[0].name"},
	} {
		_, err := config.Load(writeConfig(t, tc.body))
		assertErrorNames(t, err, tc.key)
	}
}

func TestOmittedKeysTakeTheSpecDefaults(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "[defaults.budget_usd]\nM = 20\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		key       string
		got, want any
	}{
		{"daemon.data_dir", cfg.Daemon.DataDir, "~/.local/share/istari"},
		{"daemon.max_concurrent_runs", cfg.Daemon.MaxConcurrentRuns, 4},
		{"defaults.max_review_rounds", cfg.Defaults.MaxReviewRounds, 3},
		{"defaults.max_attempts", cfg.Defaults.MaxAttempts, 3},
		{"defaults.plan_gate", cfg.Defaults.PlanGate, false},
		{"defaults.priority", cfg.Defaults.Priority, "P2"},
		{"defaults.zones", cfg.Defaults.Zones, "serialize"},
		{"defaults.merge.mode", cfg.Defaults.Merge.Mode, "after_approval"},
		{"defaults.merge.auto_max_size", cfg.Defaults.Merge.AutoMaxSize, "S"},
		{"defaults.merge.method", cfg.Defaults.Merge.Method, "squash"},
		{"defaults.budget_usd.XS", cfg.Defaults.BudgetUSD["XS"], 2.0},
		{"defaults.budget_usd.S", cfg.Defaults.BudgetUSD["S"], 5.0},
		{"defaults.budget_usd.M", cfg.Defaults.BudgetUSD["M"], 20.0},
		{"defaults.budget_usd.L", cfg.Defaults.BudgetUSD["L"], 30.0},
		{"tui.theme", cfg.TUI.Theme, "terminal"},
		{"report.at", cfg.Report.At, "08:00"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.key, c.got, c.want)
		}
	}
}

func TestDataDirExpandsTheHomeDirectory(t *testing.T) {
	t.Setenv("HOME", "/home/alex")
	for in, want := range map[string]string{
		"~/.local/share/istari": "/home/alex/.local/share/istari",
		"/var/lib/istari":       "/var/lib/istari",
		"~":                     "/home/alex",
	} {
		got, err := config.Daemon{DataDir: in}.DataDirPath()
		if err != nil || got != want {
			t.Errorf("DataDirPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
