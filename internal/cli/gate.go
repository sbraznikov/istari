package cli

import (
	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
)

// newGateCommand returns istari gate list|approve (spec section 9): the
// tasks a human performs, such as approving a plan (plan_gate).
func newGateCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "gate", Short: "Gates for a human: list, approve", Args: Args(cobra.NoArgs)}
	header := []string{"GATE", "TICKET", "STATE"}
	rows := func(tasks ...core.Task) [][]string {
		out := make([][]string, len(tasks))
		for i, t := range tasks {
			out[i] = []string{t.ID, t.Ticket, string(t.State)}
		}
		return out
	}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List the gates waiting for a human", Args: Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var gates []core.Task
			if err := opts.call(cmd, "gate.list", "", api.Empty{}, &gates); err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts, gates, header, rows(gates...))
		},
	})
	cmd.AddCommand(writeCommand(opts, &cobra.Command{
		Use: "approve <gate>", Short: "Approve a gate (humans only)", Args: Args(cobra.ExactArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		var gate core.Task
		if err := opts.call(cmd, "gate.approve", key, api.TaskActRequest{Actor: opts.Actor, ID: args[0]}, &gate); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, gate, header, rows(gate))
	}))
	return cmd
}
