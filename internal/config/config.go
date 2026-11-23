// Package config loads the installation config, ~/.config/istari/config.toml
// (spec section 12).
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the installation config.
type Config struct {
	Daemon   Daemon    `toml:"daemon"`
	Defaults Defaults  `toml:"defaults"`
	Slack    Slack     `toml:"slack"`
	TUI      TUI       `toml:"tui"`
	Report   Report    `toml:"report"`
	Projects []Project `toml:"project"`
}

// Daemon configures the long-running istari process.
type Daemon struct {
	DataDir           string `toml:"data_dir"`
	MaxConcurrentRuns int    `toml:"max_concurrent_runs"`
}

// Defaults apply to every project; .istari/pipeline.toml overrides them.
type Defaults struct {
	MaxReviewRounds int                `toml:"max_review_rounds"`
	MaxAttempts     int                `toml:"max_attempts"`
	PlanGate        bool               `toml:"plan_gate"`
	Priority        string             `toml:"priority"`
	Zones           string             `toml:"zones"`
	Merge           Merge              `toml:"merge"`
	BudgetUSD       map[string]float64 `toml:"budget_usd"`
}

// Merge sets how pull requests are merged.
type Merge struct {
	Mode        string `toml:"mode"`
	AutoMaxSize string `toml:"auto_max_size"`
	Method      string `toml:"method"`
}

// Slack holds the Slack app tokens and the operations channel.
// Token values may be references such as "env:SLACK_BOT_TOKEN".
type Slack struct {
	AppToken   string `toml:"app_token"`
	BotToken   string `toml:"bot_token"`
	OpsChannel string `toml:"ops_channel"`
}

// TUI configures `istari top`.
type TUI struct {
	Theme string `toml:"theme"`
}

// Report configures the morning report.
type Report struct {
	At string `toml:"at"`
}

// Project is one [[project]] entry.
type Project struct {
	Name    string        `toml:"name"`
	Repo    string        `toml:"repo"`
	Tickets Tickets       `toml:"tickets"`
	Slack   ProjectSlack  `toml:"slack"`
	Repos   []ProjectRepo `toml:"repos"`
}

// Tickets selects the ticket system of a project.
type Tickets struct {
	Provider string `toml:"provider"`
}

// ProjectSlack names the Slack channels and owner of a project.
type ProjectSlack struct {
	Work      string `toml:"work"`
	Decisions string `toml:"decisions"`
	Owner     string `toml:"owner"`
}

// ProjectRepo is one repository of a multi-repository project.
type ProjectRepo struct {
	Repo     string `toml:"repo"`
	Primary  bool   `toml:"primary"`
	DoneWhen string `toml:"done_when"`
}

// DefaultPath returns ~/.config/istari/config.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "istari", "config.toml"), nil
}

// defaults returns the values that apply when a key is omitted. They are the
// values of the example in spec section 12.
func defaults() Config {
	return Config{
		Daemon: Daemon{DataDir: "~/.local/share/istari", MaxConcurrentRuns: 4},
		Defaults: Defaults{
			MaxReviewRounds: 3,
			MaxAttempts:     3,
			Priority:        "P2",
			Zones:           "serialize",
			Merge:           Merge{Mode: "after_approval", AutoMaxSize: "S", Method: "squash"},
			BudgetUSD:       map[string]float64{"XS": 2, "S": 5, "M": 15, "L": 30},
		},
		TUI:    TUI{Theme: "terminal"},
		Report: Report{At: "08:00"},
	}
}

// Load reads and validates the config file at path. Omitted keys keep their
// defaults; a [defaults.budget_usd] table overrides only the sizes it names.
func Load(path string) (*Config, error) {
	cfg := defaults()
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("config %s: unknown key %s", path, strings.Join(keys, ", "))
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// Scales from spec sections 4.7 (sizes) and 4.9 (priorities). XL never goes
// into work, so it is not a valid auto-merge limit or budget row.
var (
	workSizes    = []string{"XS", "S", "M", "L"}
	priorities   = []string{"P0", "P1", "P2", "P3"}
	zoneModes    = []string{"serialize", "warn", "off"}
	mergeModes   = []string{"manual", "after_approval", "auto"}
	mergeMethods = []string{"squash", "merge", "rebase"}
)

func (c *Config) validate() error {
	var errs []error
	fail := func(key, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{key}, args...)...))
	}
	oneOf := func(key, value string, allowed []string) {
		if !slices.Contains(allowed, value) {
			fail(key, "%q is not one of %s", value, strings.Join(allowed, ", "))
		}
	}
	positive := func(key string, value int) {
		if value < 1 {
			fail(key, "must be positive, got %d", value)
		}
	}

	positive("daemon.max_concurrent_runs", c.Daemon.MaxConcurrentRuns)
	positive("defaults.max_review_rounds", c.Defaults.MaxReviewRounds)
	positive("defaults.max_attempts", c.Defaults.MaxAttempts)
	oneOf("defaults.priority", c.Defaults.Priority, priorities)
	oneOf("defaults.zones", c.Defaults.Zones, zoneModes)
	oneOf("defaults.merge.mode", c.Defaults.Merge.Mode, mergeModes)
	oneOf("defaults.merge.auto_max_size", c.Defaults.Merge.AutoMaxSize, workSizes)
	oneOf("defaults.merge.method", c.Defaults.Merge.Method, mergeMethods)
	for _, size := range slices.Sorted(maps.Keys(c.Defaults.BudgetUSD)) {
		key := "defaults.budget_usd." + size
		oneOf(key, size, workSizes)
		if c.Defaults.BudgetUSD[size] <= 0 {
			fail(key, "must be positive")
		}
	}
	if _, err := time.Parse("15:04", c.Report.At); err != nil {
		fail("report.at", "%q is not a time of day HH:MM", c.Report.At)
	}

	names := map[string]bool{}
	for i, p := range c.Projects {
		key := fmt.Sprintf("project[%d].name", i)
		switch {
		case p.Name == "":
			fail(key, "is required")
		case names[p.Name]:
			fail(key, "%q is used by another project", p.Name)
		}
		names[p.Name] = true
	}
	return errors.Join(errs...)
}
