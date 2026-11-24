package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/harness"
)

// newDoctorCommand returns istari doctor (spec section 9). It checks what is
// built so far: the registered harnesses. Slack, the tracker and VCS checks
// come with their tickets.
func newDoctorCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that istari can run here: harnesses",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			type check struct {
				Name   string `json:"name"`
				OK     bool   `json:"ok"`
				Detail string `json:"detail,omitempty"`
			}
			var checks []check
			failed := 0
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
				return errors.New(fmt.Sprintf("%d check(s) failed", failed))
			}
			return nil
		},
	}
}
