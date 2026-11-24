package cli

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
)

func taskRows(tasks ...core.Task) [][]string {
	rows := make([][]string, len(tasks))
	for i, t := range tasks {
		rows[i] = []string{t.ID, string(t.State), t.Role, t.Ticket, orDash(t.Owner), timeOrDash(t.LeaseUntil),
			strconv.Itoa(t.Attempts) + "/" + strconv.Itoa(t.MaxAttempts)}
	}
	return rows
}

var taskHeader = []string{"ID", "STATE", "ROLE", "TICKET", "OWNER", "LEASE UNTIL", "ATTEMPTS"}

func newTaskCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Tasks: list, show, claim and the owner's lease actions", Args: Args(cobra.NoArgs)}

	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List tasks", Args: Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var tasks []core.Task
			if err := opts.call(cmd, "task.list", "", api.Empty{}, &tasks); err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts, tasks, taskHeader, taskRows(tasks...))
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "show <id>", Short: "Show a task", Args: Args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var task core.Task
			if err := opts.call(cmd, "task.show", "", api.TaskShowRequest{ID: args[0]}, &task); err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts, task, taskHeader, taskRows(task))
		},
	})

	var ready bool
	var role string
	claim := writeCommand(opts, &cobra.Command{
		Use:   "claim [<id> | --ready [--role <role>]]",
		Short: "Claim a task and print its lease; keep the token for the owner's actions",
		Args:  Args(cobra.MaximumNArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		req := api.TaskClaimRequest{Actor: opts.Actor, Role: role}
		switch {
		case len(args) == 1 && !ready && role == "":
			req.ID = args[0]
		case len(args) == 0 && ready:
		default:
			return usageError{errors.New("name a task, or pass --ready (optionally with --role)")}
		}
		var lease core.Lease
		if err := opts.call(cmd, "task.claim", key, req, &lease); err != nil {
			return err
		}
		return printLease(cmd, opts, lease)
	})
	claim.Flags().BoolVar(&ready, "ready", false, "claim the oldest ready task")
	claim.Flags().StringVar(&role, "role", "", "with --ready: only tasks of this role")
	cmd.AddCommand(claim)

	cmd.AddCommand(leaseCommand(opts, "renew", "Renew the lease", "", "task.renew", true))
	cmd.AddCommand(leaseCommand(opts, "release", "Give the task back without using an attempt", "", "task.release", false))
	cmd.AddCommand(leaseCommand(opts, "complete", "Complete the task", "", "task.complete", false))
	cmd.AddCommand(leaseCommand(opts, "fail", "Record a failed attempt", "reason", "task.fail", false))
	cmd.AddCommand(leaseCommand(opts, "block", "Block the task: needs_input, awaiting_approval, budget_exceeded or ci_red", "kind", "task.block", false))

	cmd.AddCommand(writeCommand(opts, &cobra.Command{
		Use: "unblock <id>", Short: "Return a blocked task to ready", Args: Args(cobra.ExactArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		var res api.OK
		if err := opts.call(cmd, "task.unblock", key, api.TaskActRequest{Actor: opts.Actor, ID: args[0]}, &res); err != nil {
			return err
		}
		return printOK(cmd, opts, res, "unblocked "+args[0])
	}))
	return cmd
}

// leaseCommand is an owner action: it presents the lease (--token, as the
// actor) and, for fail and block, one extra flag.
func leaseCommand(opts *Options, name, short, extra, op string, returnsLease bool) *cobra.Command {
	var token, value string
	cmd := writeCommand(opts, &cobra.Command{
		Use: name + " <id> --token <token>", Short: short, Args: Args(cobra.ExactArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		if token == "" {
			return usageError{errors.New("--token is required: it is printed by task claim")}
		}
		if extra == "kind" && value == "" {
			return usageError{errors.New("--kind is required")}
		}
		req := api.LeaseRequest{Lease: core.Lease{Task: args[0], Owner: opts.Actor, Token: token}}
		switch extra {
		case "reason":
			req.Reason = value
		case "kind":
			req.Kind = value
		}
		if returnsLease {
			var lease core.Lease
			if err := opts.call(cmd, op, key, req, &lease); err != nil {
				return err
			}
			return printLease(cmd, opts, lease)
		}
		var res api.OK
		if err := opts.call(cmd, op, key, req, &res); err != nil {
			return err
		}
		return printOK(cmd, opts, res, fmt.Sprintf("%s %s", name, args[0]))
	})
	cmd.Flags().StringVar(&token, "token", "", "the lease token printed by task claim")
	if extra != "" {
		cmd.Flags().StringVar(&value, extra, "", extra)
	}
	return cmd
}

func printLease(cmd *cobra.Command, opts *Options, l core.Lease) error {
	return render(cmd.OutOrStdout(), opts, l, []string{"TASK", "OWNER", "TOKEN", "UNTIL"},
		[][]string{{l.Task, l.Owner, l.Token, timeOrDash(l.Until)}})
}

func printOK(cmd *cobra.Command, opts *Options, res api.OK, text string) error {
	if opts.JSON {
		return render(cmd.OutOrStdout(), opts, res, nil, nil)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), text)
	return err
}
