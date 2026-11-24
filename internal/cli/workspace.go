package cli

import (
	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/runner"
)

var workspaceHeader = []string{"TICKET", "STATE", "BRANCH", "DIR", "REASON"}

func workspaceRows(list []runner.Workspace) [][]string {
	rows := make([][]string, len(list))
	for i, w := range list {
		rows[i] = []string{orDash(w.Ticket), w.State, orDash(w.Branch), w.Dir, orDash(w.Reason)}
	}
	return rows
}

// newWorkspaceCommand returns istari workspace list|prune (spec section 9):
// the ticket worktrees.
func newWorkspaceCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "workspace", Short: "Ticket worktrees: list, prune", Args: Args(cobra.NoArgs)}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List the ticket worktrees", Args: Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var list []runner.Workspace
			if err := opts.call(cmd, "workspace.list", "", api.Empty{}, &list); err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts, list, workspaceHeader, workspaceRows(list))
		},
	})
	cmd.AddCommand(writeCommand(opts, &cobra.Command{
		Use:   "prune",
		Short: "Remove worktrees without a ticket or of a finished ticket; unsaved work is kept",
		Args:  Args(cobra.NoArgs),
	}, func(cmd *cobra.Command, _ []string, key string) error {
		var pruned []runner.Workspace
		if err := opts.call(cmd, "workspace.prune", key, api.ActorRequest{Actor: opts.Actor}, &pruned); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, pruned, workspaceHeader, workspaceRows(pruned))
	}))
	return cmd
}
