package cli

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/github/ratelimit"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/standard"
	"github.com/sbraznikov/istari/internal/vcs"
)

func newDaemonCommand(opts *Options) *cobra.Command {
	var paused bool
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the istari daemon in the foreground",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts.Config)
			if err != nil {
				return err
			}
			dataDir, err := cfg.Daemon.DataDirPath()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, os.Interrupt)
			defer stop()
			daemonOpts := api.DaemonOptions{DataDir: dataDir, Limits: limitsOf(cfg), Priority: cfg.Defaults.Priority, Repos: syncRepos(cfg), GitServer: os.Getenv("GITHUB_SERVER_URL")}
			// One transport under every GitHub client: istari doctor counts
			// all requests, and one quota slows them all down (#208).
			transport := ratelimit.New(ratelimit.Options{})
			if len(daemonOpts.Repos) > 0 {
				tracker, err := github.FromEnvironmentWith(ctx, &http.Client{Timeout: 30 * time.Second, Transport: transport})
				if err != nil {
					// The daemon still runs; istari doctor and istari sync say why
					// tickets are not synced.
					log.Printf("istari: ticket sync is off: %v", err)
				} else {
					daemonOpts.Tracker = tracker
					daemonOpts.Standards = standard.ForTracker{Tracker: tracker, DefaultPriority: cfg.Defaults.Priority}
				}
			}
			if len(daemonOpts.Repos) > 0 {
				client, err := vcsOf(ctx, cfg)
				if err != nil {
					log.Printf("istari: pushes and pull requests are off: %v", err)
				} else {
					daemonOpts.VCS = client.WithTransport(transport)
				}
			}
			daemonOpts.Runner = runnerOf(cmd.Context(), cfg)
			if daemonOpts.Tracker != nil {
				daemonOpts.Runner.Tickets = daemonOpts.Tracker // the ticket text of the brief
			}
			daemonOpts.Defaults = cfg.Defaults
			if paused {
				if opts.Actor == "" {
					return usageError{errors.New("--paused records the pause in the name of --actor: pass a non-empty --actor")}
				}
				daemonOpts.PausedBy = opts.Actor
			}
			return api.RunDaemon(ctx, daemonOpts)
		},
	}
	cmd.Flags().BoolVar(&paused, "paused", false, "start with the hand-out of tasks paused for all projects, as istari pause --all does; tickets still sync")
	cmd.AddCommand(newDaemonInstallCommand(opts))
	return cmd
}

// syncRepos lists the repositories of the projects whose tracker is GitHub
// Issues (the default, spec section 12).
func syncRepos(cfg *config.Config) []tickets.SyncRepo {
	var repos []tickets.SyncRepo
	for _, p := range cfg.Projects {
		if p.Tickets.Provider != "" && p.Tickets.Provider != "github" {
			continue
		}
		for _, r := range p.RepoList() {
			repos = append(repos, tickets.SyncRepo{Repo: r.Repo, Project: p.Name, Trusted: p.Trusted, LabelOnly: p.Admission.OrDefault() == config.AdmissionLabel})
		}
	}
	return repos
}

// runnerOf returns the runner's slots: per role of the harnesses' default
// profiles, as many as the role's max_concurrent_runs_by_role, else one,
// named <role>-<n> (spec section 7.2). A harness that cannot run here gets no
// slots; with none, or no project, the runner is off.
func runnerOf(ctx context.Context, cfg *config.Config) runner.RunnerOptions {
	var opts runner.RunnerOptions
	for _, p := range cfg.Projects {
		if len(p.RepoList()) > 0 {
			opts.Projects = append(opts.Projects, p.Name)
		}
	}
	if len(opts.Projects) == 0 {
		return runner.RunnerOptions{}
	}
	// The git boundaries are a hook the harness runs: this binary's istari
	// hook git-guard (spec section 7.7).
	exe, err := os.Executable()
	if err != nil {
		// No run goes without its guard.
		log.Printf("istari: runner: off, the istari binary for the git guard is not known: %v", err)
		return runner.RunnerOptions{}
	}
	opts.Budgets = cfg.Defaults.BudgetUSD // every run is capped by its ticket's budget
	opts.Guard = "'" + strings.ReplaceAll(exe, "'", `'\''`) + "' hook git-guard"
	defaults := harness.Defaults()
	roles := make([]string, 0, len(defaults))
	for role := range defaults {
		roles = append(roles, role)
	}
	slices.Sort(roles)
	usable := map[string]bool{}
	for _, role := range roles {
		profile := defaults[role]
		ok, checked := usable[profile.Harness]
		if !checked {
			h, err := harness.Get(profile.Harness)
			if err == nil {
				err = h.Check(ctx)
			}
			if err != nil {
				log.Printf("istari: runner: harness %s cannot run here, its roles get no slots: %v", profile.Harness, err)
			}
			ok = err == nil
			usable[profile.Harness] = ok
		}
		if !ok {
			continue
		}
		n := max(cfg.Defaults.MaxConcurrentRunsByRole[role], 1)
		for i := 1; i <= n; i++ {
			opts.Slots = append(opts.Slots, runner.Slot{Profile: fmt.Sprintf("%s-%d", role, i), Role: role, Harness: profile})
		}
	}
	return opts
}

// vcsOf returns the GitHub side: the App of [github_app], or the gh CLI's
// token when there is none (spec section 11).
func vcsOf(ctx context.Context, cfg *config.Config) (*vcs.Client, error) {
	api := os.Getenv("GITHUB_API_URL")
	if cfg.GitHubApp.AppID > 0 {
		tokens, err := vcs.NewAppTokens(cfg.GitHubApp.AppID, cfg.GitHubApp.PrivateKey, api)
		if err != nil {
			return nil, err
		}
		return vcs.NewClient(api, tokens), nil
	}
	token, err := github.CLIToken(ctx)
	if err != nil {
		return nil, err
	}
	return vcs.NewClient(api, vcs.StaticToken(token)), nil
}

// loadConfig loads the config at path. Without an explicit path a missing
// ~/.config/istari/config.toml means an installation with the defaults.
func loadConfig(path string) (*config.Config, error) {
	explicit := path != ""
	if !explicit {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			return nil, err
		}
	}
	cfg, err := config.Load(path)
	if !explicit && errors.Is(err, fs.ErrNotExist) {
		def := config.Default()
		return &def, nil
	}
	return cfg, err
}

func newDaemonInstallCommand(opts *Options) *cobra.Command {
	kind := map[string]string{"darwin": "launchd"}[runtime.GOOS]
	if kind == "" {
		kind = "systemd"
	}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write a launchd or systemd unit for the daemon; enabling it is up to you",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			args := []string{exe, "daemon"}
			if opts.Config != "" {
				cfgPath, err := filepath.Abs(opts.Config)
				if err != nil {
					return err
				}
				args = append(args, "--config", cfgPath)
			}
			var path, unit, enable string
			switch kind {
			case "launchd":
				path = filepath.Join(home, "Library", "LaunchAgents", "dev.istari.daemon.plist")
				unit = launchdPlist(args, os.Getenv("PATH"), filepath.Join(home, "Library", "Logs", "istari.log"))
				enable = "launchctl bootstrap gui/$(id -u) " + shellQuote(path)
			case "systemd":
				path = filepath.Join(home, ".config", "systemd", "user", "istari.service")
				unit = systemdUnit(args, os.Getenv("PATH"))
				enable = "systemctl --user enable --now istari.service"
			default:
				return usageError{fmt.Errorf("--kind %q is not launchd or systemd", kind)}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
				return err
			}
			if opts.JSON {
				return render(cmd.OutOrStdout(), opts, map[string]any{"path": path, "enabled": false, "enable": enable}, nil, nil)
			}
			// A service starts with a minimal PATH: the daemon gets this shell's,
			// so it finds gh, git and the harnesses as you do.
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s with the PATH of this shell\nIt is not enabled. To enable and start it, run:\n  %s\n", path, enable)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", kind, "unit kind: launchd or systemd")
	return cmd
}

func launchdPlist(args []string, path, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.istari.daemon</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	fmt.Fprintf(&b, `  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>EnvironmentVariables</key>
  <dict>
      <key>PATH</key>
      <string>%s</string>
  </dict>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, xmlEscape(path), xmlEscape(logPath))
	return b.String()
}

func systemdUnit(args []string, path string) string {
	// Quoted for systemd: backslash and quote escaped, % (specifiers) and
	// $ (variables) doubled.
	quote := func(s string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(s) + `"`
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quote(a)
	}
	return fmt.Sprintf(`[Unit]
Description=istari daemon

[Service]
Environment=%s
ExecStart=%s
Restart=on-failure

[Install]
WantedBy=default.target
`, quote("PATH="+path), strings.Join(quoted, " "))
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// limitsOf collects the concurrency limits of the config.
func limitsOf(cfg *config.Config) core.Limits {
	l := core.Limits{Total: cfg.Daemon.MaxConcurrentRuns, PerProject: map[string]int{}, PerRole: cfg.Defaults.MaxConcurrentRunsByRole}
	for _, p := range cfg.Projects {
		if p.MaxConcurrentRuns != nil {
			l.PerProject[p.Name] = *p.MaxConcurrentRuns
		}
	}
	return l
}
