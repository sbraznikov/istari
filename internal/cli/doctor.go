package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/tickets/github"
)

// newDoctorCommand returns istari doctor (spec section 9). It checks what is
// built so far: the tracker token, when a project syncs tickets, and the
// registered harnesses. Slack and VCS checks come with their tickets.
func newDoctorCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that istari can run here: tracker and harnesses",
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
			}
			for _, h := range harness.All() {
				c := check{Name: "harness " + h.Name(), OK: true}
				if err := h.Check(cmd.Context()); err != nil {
					c.OK, c.Detail = false, err.Error()
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
