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

// newRecoverCommand returns istari recover: rebuild what istari lost with
// its database from the tracker (spec section 4.6).
func newRecoverCommand(opts *Options) *cobra.Command {
	return writeCommand(opts, &cobra.Command{
		Use:   "recover",
		Short: "Restore lost tickets, their states, holds and journals from the tracker",
		Args:  Args(cobra.NoArgs),
	}, func(cmd *cobra.Command, _ []string, key string) error {
		var r tickets.SyncReport
		if err := opts.call(cmd, "recover", key, api.ActorRequest{Actor: opts.Actor}, &r); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, r, []string{"RECOVERED"}, [][]string{{strconv.Itoa(r.Recovered)}})
	})
}
