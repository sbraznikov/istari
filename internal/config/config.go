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
	Daemon    Daemon    `toml:"daemon"`
	Defaults  Defaults  `toml:"defaults"`
	GitHubApp GitHubApp `toml:"github_app"`
	Slack     Slack     `toml:"slack"`
	TUI       TUI       `toml:"tui"`
	Report    Report    `toml:"report"`
	Projects  []Project `toml:"project"`
}

// GitHubApp is the GitHub App istari pushes, opens pull requests and reads
// checks as (spec section 12). Without it the gh CLI's token is used.
type GitHubApp struct {
	AppID      int64  `toml:"app_id"`
	PrivateKey string `toml:"private_key"` // a path to the App's PEM key
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
	// MaxConcurrentRunsByRole limits claimed tasks at once per role; a role
	// that is not listed has no limit of its own.
	MaxConcurrentRunsByRole map[string]int `toml:"max_concurrent_runs_by_role"`
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
	// Trusted are the tracker logins whose tickets are admitted without a
	// human and who may admit others' (spec section 4.5). Empty: everyone
	// with write access to the repository.
	Trusted []string `toml:"trusted"`
	// MaxConcurrentRuns limits claimed tasks at once in this project; nil
	// (the key absent) means no limit of its own.
	MaxConcurrentRuns *int `toml:"max_concurrent_runs"`
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
	DoneWhen string `toml:"done_when"` // merged (default) or released
	// Link is how the repository is linked to the primary one (spec section
	// 4.4): package, submodule, path or none. With none, soft dependencies
	// are unavailable until a human describes the link.
	Link string `toml:"link"`
}

// RepoList returns the project's repositories, the single-repository form
// (repo = "...") included as the primary one.
func (p Project) RepoList() []ProjectRepo {
	if len(p.Repos) == 0 && p.Repo != "" {
		return []ProjectRepo{{Repo: p.Repo, Primary: true}}
	}
	return p.Repos
}

// DataDirPath returns daemon.data_dir with a leading ~ expanded to the home directory.
func (d Daemon) DataDirPath() (string, error) {
	if d.DataDir == "~" || strings.HasPrefix(d.DataDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(d.DataDir, "~")), nil
	}
	return d.DataDir, nil
}

// DefaultPath returns ~/.config/istari/config.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "istari", "config.toml"), nil
}

// Default returns the config of an installation without a config file: the
// values that apply when a key is omitted, which are those of the example in
// spec section 12.
func Default() Config {
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
	cfg := Default()
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
	for _, role := range slices.Sorted(maps.Keys(c.Defaults.MaxConcurrentRunsByRole)) {
		if c.Defaults.MaxConcurrentRunsByRole[role] < 1 {
			fail("defaults.max_concurrent_runs_by_role."+role, "must be positive, got %d", c.Defaults.MaxConcurrentRunsByRole[role])
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
		if p.Repo != "" && len(p.Repos) > 0 {
			fail(fmt.Sprintf("project[%d].repo", i), "set either repo or [[project.repos]], not both")
		}
		for j, r := range p.Repos {
			rkey := fmt.Sprintf("project[%d].repos[%d]", i, j)
			if r.DoneWhen != "" {
				oneOf(rkey+".done_when", r.DoneWhen, []string{"merged", "released"})
			}
			if r.Link != "" {
				oneOf(rkey+".link", r.Link, []string{"package", "submodule", "path", "none"})
			}
		}
		if p.MaxConcurrentRuns != nil && *p.MaxConcurrentRuns < 1 {
			fail(fmt.Sprintf("project[%d].max_concurrent_runs", i), "must be positive, got %d", *p.MaxConcurrentRuns)
		}
	}
	return errors.Join(errs...)
}
