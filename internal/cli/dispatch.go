package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/scheduler"
)

// newPauseCommands returns istari pause and istari resume (spec section 9:
// pause|resume [--all|--project]).
func newPauseCommands(opts *Options) []*cobra.Command {
	var cmds []*cobra.Command
	for _, c := range []struct{ name, short, op, done string }{
		{"pause", "Stop handing out tasks; running tasks are not affected", "dispatch.pause", "paused"},
		{"resume", "Restart handing out tasks", "dispatch.resume", "resumed"},
	} {
		var all bool
		var project string
		cmd := writeCommand(opts, &cobra.Command{
			Use: c.name + " --all | --project <name>", Short: c.short, Args: Args(cobra.NoArgs),
		}, func(cmd *cobra.Command, _ []string, key string) error {
			if all == (project != "") {
				return usageError{errors.New("pass exactly one of --all and --project")}
			}
			var res api.OK
			if err := opts.call(cmd, c.op, key, api.DispatchRequest{Actor: opts.Actor, Project: project}, &res); err != nil {
				return err
			}
			scope := "all projects"
			if project != "" {
				scope = "project " + project
			}
			return printOK(cmd, opts, res, fmt.Sprintf("%s the hand-out of tasks for %s", c.done, scope))
		})
		cmd.Flags().BoolVar(&all, "all", false, "all projects")
		cmd.Flags().StringVar(&project, "project", "", "one project")
		cmds = append(cmds, cmd)
	}
	return cmds
}

// newScheduleCommand returns istari schedule: a delayed API write.
func newScheduleCommand(opts *Options) *cobra.Command {
	var in time.Duration
	cmd := writeCommand(opts, &cobra.Command{
		Use:   "schedule --in <duration> <operation> [<json request>]",
		Short: "Perform an API write later, once, e.g. schedule --in 30m task.unblock '{\"id\":\"t_1\"}'",
		Args:  Args(cobra.RangeArgs(1, 2)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		if in <= 0 {
			return usageError{errors.New("--in must be a positive duration")}
		}
		request := map[string]any{}
		if len(args) == 2 {
			if err := json.Unmarshal([]byte(args[1]), &request); err != nil {
				return usageError{fmt.Errorf("the request is not a JSON object: %w", err)}
			}
		}
		if _, ok := request["actor"]; !ok {
			request["actor"] = opts.Actor // the action acts as whoever scheduled it
		}
		raw, _ := json.Marshal(request)
		var timer scheduler.Timer
		req := api.TimerScheduleRequest{Actor: opts.Actor, At: time.Now().Add(in), Op: args[0], Request: raw}
		if err := opts.call(cmd, "timer.schedule", key, req, &timer); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, timer, []string{"TIMER", "AT", "OPERATION", "STATE"},
			[][]string{{timer.ID, timeOrDash(timer.At), timer.Op, string(timer.State)}})
	})
	cmd.Flags().DurationVar(&in, "in", 0, "how long from now")
	return cmd
}
