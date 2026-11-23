package cli

import (
	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
)

var ticketHeader = []string{"TICKET", "STATE", "HOLD", "PROJECT", "UPDATED"}

func ticketRows(tickets ...core.Ticket) [][]string {
	rows := make([][]string, len(tickets))
	for i, t := range tickets {
		rows[i] = []string{t.Ref, string(t.State), orDash(t.Hold), orDash(t.Project), timeOrDash(t.Updated)}
	}
	return rows
}

func newTicketCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "ticket", Short: "Tickets: list, show, pause, resume, cancel, reopen, retry", Args: Args(cobra.NoArgs)}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List tickets", Args: Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var tickets []core.Ticket
			if err := opts.call(cmd, "ticket.list", "", api.Empty{}, &tickets); err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), opts, tickets, ticketHeader, ticketRows(tickets...))
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "show <ref>", Short: "Show a ticket", Args: Args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ticket core.Ticket
			if err := opts.call(cmd, "ticket.show", "", api.TicketShowRequest{Ref: args[0]}, &ticket); err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), opts, ticket, ticketHeader, ticketRows(ticket))
		},
	})
	// The human actions on a ticket (spec sections 4.1 and 10.5).
	for _, a := range []struct {
		name, short, op string
		req             func(actor, ref string) any
	}{
		{"pause", "Put the ticket on hold (paused)", "ticket.hold", func(actor, ref string) any {
			return api.TicketHoldRequest{Actor: actor, Ref: ref, Reason: "paused"}
		}},
		{"resume", "Lift a paused hold", "ticket.unhold", func(actor, ref string) any {
			return api.TicketHoldRequest{Actor: actor, Ref: ref, Reason: "paused"}
		}},
		{"retry", "Lift an attempts_exhausted hold", "ticket.unhold", func(actor, ref string) any {
			return api.TicketHoldRequest{Actor: actor, Ref: ref, Reason: "attempts_exhausted"}
		}},
		{"cancel", "Cancel the ticket (T13)", "ticket.fire", func(actor, ref string) any {
			return api.TicketFireRequest{Actor: actor, Ref: ref, Trigger: "cancel"}
		}},
		{"reopen", "Reopen a canceled ticket (T14)", "ticket.fire", func(actor, ref string) any {
			return api.TicketFireRequest{Actor: actor, Ref: ref, Trigger: "reopen"}
		}},
	} {
		cmd.AddCommand(writeCommand(opts, &cobra.Command{
			Use: a.name + " <ref>", Short: a.short, Args: Args(cobra.ExactArgs(1)),
		}, func(cmd *cobra.Command, args []string, key string) error {
			var ticket core.Ticket
			if err := opts.call(cmd, a.op, key, a.req(opts.Actor, args[0]), &ticket); err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), opts, ticket, ticketHeader, ticketRows(ticket))
		}))
	}
	return cmd
}
