package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets/github"
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
	cmd := &cobra.Command{Use: "ticket", Short: "Tickets: list, show, check, pause, resume, cancel, reopen, retry", Args: Args(cobra.NoArgs)}
	cmd.AddCommand(newTicketCheckCommand(opts), newTicketCreateCommand(opts), newTicketDepsCommand(opts))
	cmd.AddCommand(writeCommand(opts, &cobra.Command{
		Use:   "ready <ref>",
		Short: "Admit a ticket to work (trusted members; the ticket must meet the standard)",
		Args:  Args(cobra.ExactArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		actor := opts.Actor
		if !cmd.Flags().Changed("actor") && os.Getenv("ISTARI_ACTOR") == "" {
			// Trust is a tracker login, not the local user name.
			login, err := github.Login(cmd.Context())
			if err != nil {
				return err
			}
			actor = "human:" + login
		}
		var ticket core.Ticket
		if err := opts.call(cmd, "ticket.ready", key, api.TicketReadyRequest{Actor: actor, Ref: args[0]}, &ticket); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, ticket, ticketHeader, ticketRows(ticket))
	}))
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List tickets", Args: Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var tickets []core.Ticket
			if err := opts.call(cmd, "ticket.list", "", api.Empty{}, &tickets); err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts, tickets, ticketHeader, ticketRows(tickets...))
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "show <ref>", Short: "Show a ticket", Args: Args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ticket core.Ticket
			if err := opts.call(cmd, "ticket.show", "", api.TicketShowRequest{Ref: args[0]}, &ticket); err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts, ticket, ticketHeader, ticketRows(ticket))
		},
	})
	cmd.AddCommand(writeCommand(opts, &cobra.Command{
		Use:   "priority <ref> <P0|P1|P2|P3>",
		Short: "Set the ticket's priority (humans only)",
		Args:  Args(cobra.ExactArgs(2)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		var ticket core.Ticket
		if err := opts.call(cmd, "ticket.priority", key, api.TicketPriorityRequest{Actor: opts.Actor, Ref: args[0], Priority: args[1]}, &ticket); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, ticket, ticketHeader, ticketRows(ticket))
	}))
	// The human actions on a ticket (spec sections 4.1 and 10.5).
	for _, a := range []struct {
		name, short, op string
		req             func(actor, ref string) any
	}{
		{"pause", "Put the ticket on hold (paused)", "ticket.hold", func(actor, ref string) any {
			return api.TicketHoldRequest{Actor: actor, Ref: ref, Reason: "paused"}
		}},
		{"resume", "Continue an unfinished ticket: lift a paused hold, take back tasks whose run is gone", "ticket.resume", func(actor, ref string) any {
			return api.TicketReadyRequest{Actor: actor, Ref: ref}
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
			return render(cmd.OutOrStdout(), opts, ticket, ticketHeader, ticketRows(ticket))
		}))
	}
	cmd.AddCommand(writeCommand(opts, &cobra.Command{
		Use:   "retry <ref>",
		Short: "Restart the ticket's failed tasks and lift its attempts_exhausted hold",
		Args:  Args(cobra.ExactArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		ref := args[0]
		var ticket core.Ticket
		if err := opts.call(cmd, "ticket.show", "", api.TicketShowRequest{Ref: ref}, &ticket); err != nil {
			return err
		}
		if ticket.Hold != "attempts_exhausted" {
			return refusal{api.ErrorBody{Code: core.CodeInvalidState, Message: fmt.Sprintf("ticket %s is not on hold for attempts_exhausted", ref)}}
		}
		var tasks []core.Task
		if err := opts.call(cmd, "task.list", "", api.Empty{}, &tasks); err != nil {
			return err
		}
		// Restart the tasks first and lift the hold last, so an interrupted
		// retry leaves the ticket held; a repeat finishes it. Each step has
		// its own key derived from the command's.
		for _, task := range tasks {
			if task.Ticket != ref || task.State != core.TaskFailed {
				continue
			}
			if err := opts.call(cmd, "task.retry", subKey(key, task.ID), api.TaskActRequest{Actor: opts.Actor, ID: task.ID}, &api.OK{}); err != nil {
				return err
			}
		}
		req := api.TicketHoldRequest{Actor: opts.Actor, Ref: ref, Reason: "attempts_exhausted"}
		if err := opts.call(cmd, "ticket.unhold", subKey(key, "hold"), req, &ticket); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, ticket, ticketHeader, ticketRows(ticket))
	}))
	return cmd
}

// subKey derives the key of one step of a command from the command's key.
func subKey(key, step string) string {
	if key == "" {
		return ""
	}
	return key + ":" + step
}
