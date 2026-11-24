package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/tickets"
)

// newSyncCommand returns istari sync: poll the trackers now instead of
// waiting for the next 5-minute poll.
func newSyncCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Sync tickets with the tracker now",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var r tickets.SyncReport
			if err := opts.call(cmd, "sync.run", "", api.Empty{}, &r); err != nil {
				return err
			}
			n := func(i int) string { return strconv.Itoa(i) }
			return render(cmd.OutOrStdout(), opts, r, []string{"CREATED", "PUSHED", "TRANSITIONED", "REVERTED", "COMMENTS"},
				[][]string{{n(r.Created), n(r.Pushed), n(r.Transitioned), n(r.Reverted), n(r.Comments)}})
		},
	}
}
