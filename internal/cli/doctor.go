package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/project"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/vcs"
)

// newDoctorCommand returns istari doctor (spec section 9). It checks what is
// built so far: the tracker token, when a project syncs tickets, and the
// registered harnesses. Slack and VCS checks come with their tickets.
func newDoctorCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that istari can run here: tracker, harnesses and worktrees",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			type check = doctorCheck
			var checks []check
			failed := 0
			cfg, cfgErr := loadConfig(opts.Config)
			if cfgErr == nil {
				c := check{Name: "daemon socket", OK: true}
				dataDir, err := cfg.Daemon.DataDirPath()
				socket := ""
				if err == nil {
					socket, err = api.SocketPath(dataDir)
				}
				c.Detail = socket
				if err != nil {
					c.OK, c.Detail = false, err.Error()
					failed++
				}
				checks = append(checks, c)
			}
			if cfgErr == nil && len(syncRepos(cfg)) > 0 {
				c := check{Name: "tracker github", OK: true}
				if _, err := github.FromEnvironment(cmd.Context()); err != nil {
					c.OK, c.Detail = false, err.Error()
					failed++
				}
				checks = append(checks, c)
				// Pushes and pull requests: the GitHub App, or the gh CLI's
				// token as that user (spec section 11).
				v := check{Name: "vcs github", OK: true}
				switch {
				case cfg.GitHubApp.AppID == 0:
					// Without an App the token is the gh CLI's (spec 11).
					if _, err := github.CLIToken(cmd.Context()); err != nil {
						v.OK, v.Detail = false, "no [github_app], and "+err.Error()
						failed++
					} else {
						v.Detail = "no [github_app]: pushes and pull requests are made as the gh CLI's user"
					}
				default:
					if _, err := vcs.NewAppTokens(cfg.GitHubApp.AppID, cfg.GitHubApp.PrivateKey, ""); err != nil {
						v.OK, v.Detail = false, err.Error()
						failed++
					} else {
						v.Detail = fmt.Sprintf("GitHub App %d", cfg.GitHubApp.AppID)
					}
				}
				checks = append(checks, v)
			}
			for _, h := range harness.All() {
				c := check{Name: "harness " + h.Name(), OK: true}
				if err := h.Check(cmd.Context()); err != nil {
					c.OK, c.Detail = false, err.Error()
					failed++
				}
				checks = append(checks, c)
			}
			// The pipeline of the repository the command runs in (spec 12).
			if c, ok := pipelineCheck(); ok {
				if !c.OK {
					failed++
				}
				checks = append(checks, c)
			}
			// The repository's standards files (spec 7.6): a missing one
			// only warns, so it does not count as failed.
			if c, ok := standardsCheck(); ok {
				checks = append(checks, c)
			}
			// Worktrees without a ticket or of a finished ticket (spec 7.2),
			// as the running daemon sees them.
			var orphans []runner.Workspace
			if err := opts.call(cmd, "workspace.orphans", "", api.Empty{}, &orphans); err == nil {
				c := check{Name: "workspaces", OK: len(orphans) == 0}
				if !c.OK {
					var names []string
					for _, o := range orphans {
						names = append(names, orDash(o.Ticket)+" "+o.Dir)
					}
					c.Detail = fmt.Sprintf("%d orphan(s), remove with istari workspace prune: %s", len(orphans), strings.Join(names, "; "))
					failed++
				}
				checks = append(checks, c)
			}
			// Slots whose harness did not start three times in a row
			// (spec 7.3), as the running daemon sees them.
			var paused []runner.PausedSlot
			if err := opts.call(cmd, "runner.slots", "", api.Empty{}, &paused); err == nil {
				c := check{Name: "runner slots", OK: len(paused) == 0}
				if !c.OK {
					var names []string
					for _, p := range paused {
						names = append(names, fmt.Sprintf("%s paused until %s: %s", p.Slot, p.Until.Local().Format("15:04:05"), p.Error))
					}
					c.Detail = strings.Join(names, "; ")
					failed++
				}
				checks = append(checks, c)
			}
			rows := make([][]string, len(checks))
			for i, c := range checks {
				status := "ok"
				switch {
				case c.Warn:
					status = "warn"
				case !c.OK:
					status = "FAIL"
				}
				rows[i] = []string{c.Name, status, orDash(c.Detail)}
			}
			if err := render(cmd.OutOrStdout(), opts, checks, []string{"CHECK", "STATUS", "DETAIL"}, rows); err != nil {
				return err
			}
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
}

// doctorCheck is one row of istari doctor. Warn marks a row that is not ok
// but does not make doctor fail.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// pipelineCheck validates .istari/pipeline.toml of the repository of the
// working directory; ok is false when there is none.
func pipelineCheck() (doctorCheck, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return doctorCheck{}, false
	}
	root, err := project.Root(dir)
	if err != nil {
		return doctorCheck{}, false
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pipeline.Path)))
	if err != nil {
		return doctorCheck{}, false
	}
	c := doctorCheck{Name: "pipeline " + pipeline.Path, OK: true}
	p, err := pipeline.Parse(data)
	if err == nil {
		err = p.Validate()
	}
	if err != nil {
		c.OK, c.Detail = false, err.Error()
	}
	return c, true
}

// standardsFiles are the files the Standards axis of the review reads
// (spec section 7.6), in the order doctor lists them.
var standardsFiles = []string{"CODING_STANDARDS.md", "CONTRIBUTING.md", "GLOSSARY.md"}

// adrDirs are the places of a repository's ADR directory (spec section 7.6).
var adrDirs = []string{"adr", filepath.Join("docs", "adr")}

// standardsCheck lists which standards files the repository of the working
// directory lacks; a missing one is a warning, not a failure. ok is false
// outside a repository.
func standardsCheck() (doctorCheck, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return doctorCheck{}, false
	}
	root, err := project.Root(dir)
	if err != nil {
		return doctorCheck{}, false
	}
	var missing []string
	for _, name := range standardsFiles {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || info.IsDir() {
			missing = append(missing, name)
		}
	}
	hasADRDir := slices.ContainsFunc(adrDirs, func(d string) bool {
		info, err := os.Stat(filepath.Join(root, d))
		return err == nil && info.IsDir()
	})
	if !hasADRDir {
		missing = append(missing, "adr/ or docs/adr/")
	}
	c := doctorCheck{Name: "standards", OK: len(missing) == 0}
	if !c.OK {
		c.Warn = true
		c.Detail = "missing: " + strings.Join(missing, ", ")
	}
	return c, true
}
