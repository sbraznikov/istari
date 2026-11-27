package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/github/ratelimit"
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
				// Who admits tickets in each project (spec 4.5).
				for _, p := range cfg.Projects {
					checks = append(checks, check{Name: "admission " + p.Name, OK: true, Detail: string(p.Admission.OrDefault())})
				}
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
				switch cfg.GitHubApp.AppID {
				case 0:
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
			// The repository the command runs in; outside one, its checks
			// are skipped.
			if root, ok := workingRoot(); ok {
				// Its pipeline (spec 12): a warning does not count as failed.
				if c, ok := pipelineCheck(root); ok {
					if !c.OK && !c.Warn {
						failed++
					}
					checks = append(checks, c)
				}
				// Its standards files (spec 7.6): a missing one only warns,
				// so it does not count as failed.
				checks = append(checks, standardsCheck(root))
			}
			// Worktrees without a ticket or of a finished ticket (spec 7.2),
			// as the running daemon sees them. One kept for unsaved work
			// only warns: prune would keep it too.
			var orphans []runner.Workspace
			if err := opts.call(cmd, "workspace.orphans", "", api.Empty{}, &orphans); err == nil {
				var removable, kept []string
				for _, o := range orphans {
					if o.State == runner.WorkspaceKept {
						kept = append(kept, orDash(o.Ticket)+" "+o.Dir+": "+o.Reason)
					} else {
						removable = append(removable, orDash(o.Ticket)+" "+o.Dir)
					}
				}
				c := check{Name: "workspaces", OK: len(removable) == 0}
				if !c.OK {
					c.Detail = fmt.Sprintf("%d orphan(s), remove with istari workspace prune: %s", len(removable), strings.Join(removable, "; "))
					failed++
				}
				checks = append(checks, c)
				if len(kept) > 0 {
					checks = append(checks, check{Name: "workspaces kept", Warn: true,
						Detail: fmt.Sprintf("%d kept for unsaved work: %s", len(kept), strings.Join(kept, "; "))})
				}
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
			// The GitHub requests of the last hour and the quota left, as
			// the running daemon counted them (#208); a low quota warns.
			var stats *ratelimit.Stats
			if err := opts.call(cmd, "github.requests", "", api.Empty{}, &stats); err == nil && stats != nil {
				checks = append(checks, githubCheck(*stats))
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

// githubKinds is the order doctor lists the kinds of GitHub requests in.
var githubKinds = []ratelimit.Kind{ratelimit.KindSync, ratelimit.KindPRWatch, ratelimit.KindJournal, ratelimit.KindLabels, ratelimit.KindFiles, ratelimit.KindOther}

// githubCheck is the row of the daemon's GitHub requests: the last hour's by
// kind, and the quota of GitHub's last answer. Fewer than ratelimit.Low left
// warns: the sync and the PR watch wait for the reset.
func githubCheck(s ratelimit.Stats) doctorCheck {
	total := 0
	var kinds []string
	for _, k := range githubKinds {
		if n := s.Requests[k]; n > 0 {
			total += n
			kinds = append(kinds, fmt.Sprintf("%s %d", k, n))
		}
	}
	detail := fmt.Sprintf("last hour %d", total)
	if len(kinds) > 0 {
		detail += " (" + strings.Join(kinds, ", ") + ")"
	}
	c := doctorCheck{Name: "github requests", OK: true}
	if !s.Known() {
		c.Detail = detail + "; quota not known yet"
		return c
	}
	c.Detail = fmt.Sprintf("%s; %d of %d left until %s", detail, s.Remaining, s.Limit, s.Reset.Local().Format("15:04"))
	if s.Remaining < ratelimit.Low {
		c.Warn = true
		c.Detail += "; the sync and the PR watch wait until then"
	}
	return c
}

// workingRoot returns the root of the repository of the working directory;
// ok is false outside a repository.
func workingRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	root, err := project.Root(dir)
	if err != nil {
		return "", false
	}
	return root, true
}

// pipelineCheck validates .istari/pipeline.toml of the repository at root;
// ok is false when there is none.
func pipelineCheck(root string) (doctorCheck, bool) {
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
	} else if w := p.Warnings(); len(w) > 0 {
		c.OK, c.Warn, c.Detail = false, true, strings.Join(w, "; ")
	}
	return c, true
}

// standardsFiles are the files the Standards axis of the review reads
// (spec section 7.6), in the order doctor lists them.
var standardsFiles = []string{"CODING_STANDARDS.md", "CONTRIBUTING.md", "GLOSSARY.md"}

// adrDirs are the places of a repository's ADR directory (spec section 7.6).
var adrDirs = []string{"adr", filepath.Join("docs", "adr")}

// standardsCheck lists which standards files the repository at root lacks;
// a missing one is a warning, not a failure.
func standardsCheck(root string) doctorCheck {
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
	return c
}
