package cli

import (
	"fmt"
	"os"
	"path/filepath"
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
			type check struct {
				Name   string `json:"name"`
				OK     bool   `json:"ok"`
				Detail string `json:"detail,omitempty"`
			}
			var checks []check
			failed := 0
			if cfg, err := loadConfig(opts.Config); err == nil && len(syncRepos(cfg)) > 0 {
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
					v.Detail = "no [github_app]: pushes and pull requests are made as the gh CLI's user, whose credentials an agent running as you can reach"
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
				checks = append(checks, check(c))
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
			rows := make([][]string, len(checks))
			for i, c := range checks {
				status := "ok"
				if !c.OK {
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

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
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
