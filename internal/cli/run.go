package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/runner"
)

var runHeader = []string{"RUN", "STATE", "PROFILE", "TICKET", "TASK", "STARTED", "REASON"}

func runRows(runs ...runner.Run) [][]string {
	rows := make([][]string, len(runs))
	for i, r := range runs {
		rows[i] = []string{r.ID, r.State, r.Profile, r.Ticket, r.Task, timeOrDash(r.Started), orDash(r.Reason)}
	}
	return rows
}

// newRunCommand returns istari run list|show|logs|stop (spec section 9).
func newRunCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "run", Short: "Agent runs: list, show, logs, stop", Args: Args(cobra.NoArgs)}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List runs", Args: Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var runs []runner.Run
			if err := opts.call(cmd, "run.list", "", api.Empty{}, &runs); err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts, runs, runHeader, runRows(runs...))
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "show <run>", Short: "Show a run", Args: Args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var run runner.Run
			if err := opts.call(cmd, "run.show", "", api.RunRequest{ID: args[0]}, &run); err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts, run, runHeader, runRows(run))
		},
	})
	var follow bool
	logs := &cobra.Command{
		Use: "logs <run>", Short: "Print a run's transcript; -f follows it until the run ends", Args: Args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var after int64
			for {
				var page api.RunLogs
				if err := opts.call(cmd, "run.logs", "", api.RunLogsRequest{ID: args[0], After: after}, &page); err != nil {
					return err
				}
				for _, line := range page.Lines {
					fmt.Fprintln(cmd.OutOrStdout(), logLine(line, opts.JSON))
				}
				after = page.Next
				if !follow || page.Ended && len(page.Lines) == 0 {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(300 * time.Millisecond):
				}
			}
		},
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing until the run ends")
	cmd.AddCommand(logs)
	cmd.AddCommand(writeCommand(opts, &cobra.Command{
		Use: "stop <run>", Short: "Stop a run; its task fails with who stopped it", Args: Args(cobra.ExactArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		var res api.OK
		if err := opts.call(cmd, "run.stop", key, api.RunActRequest{Actor: opts.Actor, ID: args[0]}, &res); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, res, []string{"OK"}, [][]string{{"stopping " + args[0]}})
	}))
	return cmd
}

// logLine renders one transcript event: as JSON, or as a short line.
func logLine(line json.RawMessage, asJSON bool) string {
	if asJSON {
		return string(line)
	}
	var e struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		Tool string `json:"tool"`
	}
	if json.Unmarshal(line, &e) != nil {
		return string(line)
	}
	switch e.Kind {
	case "tool_call":
		return "▸ " + e.Tool
	case "message":
		return strings.TrimSpace(e.Text)
	default:
		return string(line)
	}
}

// newAgentCommands returns the commands of an agent in a run (spec section
// 7.4): istari whoami and istari complete. They need ISTARI_TOKEN.
func newAgentCommands(opts *Options) []*cobra.Command {
	whoami := &cobra.Command{
		Use: "whoami", Short: "For an agent: its role, ticket, task, permissions, lease and budget", Args: Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var w api.Whoami
			if err := opts.call(cmd, "whoami", "", api.Empty{}, &w); err != nil {
				return err
			}
			rows := [][]string{
				{"profile", w.Profile}, {"role", w.Role}, {"ticket", w.Ticket}, {"task", w.Task}, {"run", w.Run},
				{"permissions", strings.Join(w.Permissions, "; ")},
				{"lease", w.LeaseRemaining + " left"},
				{"budget", budgetText(w)},
			}
			return render(cmd.OutOrStdout(), opts, w, []string{"WHAT", "VALUE"}, rows)
		},
	}
	complete := writeCommand(opts, &cobra.Command{
		Use: "complete [message]", Short: "For an agent: say the work of the task is done", Args: Args(cobra.MaximumNArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		req := api.RunCompleteRequest{Actor: opts.Actor}
		if len(args) == 1 {
			req.Message = args[0]
		}
		var res api.OK
		if err := opts.call(cmd, "run.complete", key, req, &res); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, res, []string{"OK"}, [][]string{{"the task completes when the run ends"}})
	})
	return []*cobra.Command{whoami, complete}
}

func budgetText(w api.Whoami) string {
	if w.BudgetUSD == 0 {
		return fmt.Sprintf("$%.2f spent, no run budget", w.SpentUSD)
	}
	return fmt.Sprintf("$%.2f of $%.2f spent, $%.2f left", w.SpentUSD, w.BudgetUSD, w.BudgetLeftUSD)
}
