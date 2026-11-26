package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
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
			var after runner.TranscriptPos
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
	var ciUnrelated string
	complete := writeCommand(opts, &cobra.Command{
		Use: "complete [message]", Short: "For an agent: say the work of the task is done", Args: Args(cobra.MaximumNArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		req := api.RunCompleteRequest{Actor: opts.Actor, CIUnrelated: ciUnrelated}
		switch {
		case len(args) == 1:
			req.Message = args[0]
		case ciUnrelated != "":
			req.Message = ciUnrelated
		}
		var res api.OK
		if err := opts.call(cmd, "run.complete", key, req, &res); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, res, []string{"OK"}, [][]string{{"the task completes when the run ends"}})
	})
	complete.Flags().StringVar(&ciUnrelated, "ci-unrelated", "", "for the implementer: the red CI is not caused by the change, and why; istari reruns the failed jobs once")
	checkpoint := writeCommand(opts, &cobra.Command{
		Use:   "checkpoint \"<done>; <next>\"",
		Short: "For the implementer: commit, run the checks, push and record the step in the ticket",
		Args:  Args(cobra.ExactArgs(1)),
	}, func(cmd *cobra.Command, args []string, key string) error {
		var cp journal.CheckpointEntry
		if err := opts.call(cmd, "checkpoint", key, api.CheckpointRequest{Actor: opts.Actor, Message: args[0]}, &cp); err != nil {
			return err
		}
		state := "green"
		if cp.Red {
			state = "red"
		}
		rows := [][]string{{"commit", cp.Commit}, {"checks", state}, {"files", strings.Join(cp.Files, " ")}}
		for _, n := range cp.Notes {
			rows = append(rows, []string{"note", n})
		}
		return render(cmd.OutOrStdout(), opts, cp, []string{"WHAT", "VALUE"}, rows)
	})
	refine := &cobra.Command{Use: "refine", Short: "For the refiner: submit the result of refining a ticket", Args: Args(cobra.NoArgs)}
	var file string
	submit := writeCommand(opts, &cobra.Command{
		Use: "submit [--file <result.yaml>]", Short: "Submit the verdict, size, deviations and assumptions (and a rewrite, questions or a decision card)", Args: Args(cobra.NoArgs),
	}, func(cmd *cobra.Command, _ []string, key string) error {
		data, err := readResult(cmd, file)
		if err != nil {
			return err
		}
		var ok api.OK // istari parses and judges it; a malformed result fails the run unless it is corrected
		if err := opts.call(cmd, "refine.submit", key, api.RefineSubmitRequest{Actor: opts.Actor, Result: string(data)}, &ok); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, ok, []string{"OK"}, [][]string{{"refine result applied"}})
	})
	submit.Flags().StringVar(&file, "file", "", "the result, YAML; - reads it from standard input; without it, $ISTARI_RESULT")
	refine.AddCommand(submit)
	plan := &cobra.Command{Use: "plan", Short: "For the planner: submit the plan of a ticket", Args: Args(cobra.NoArgs)}
	var planFile string
	planSubmit := writeCommand(opts, &cobra.Command{
		Use: "submit [--file <plan.yaml>]", Short: "Submit the slices, seams, change zone and, if needed, the split", Args: Args(cobra.NoArgs),
	}, func(cmd *cobra.Command, _ []string, key string) error {
		data, err := readResult(cmd, planFile)
		if err != nil {
			return err
		}
		var answer api.PlanAnswer // istari parses and checks it; a rejected plan fails the run unless it is corrected
		if err := opts.call(cmd, "plan.submit", key, api.PlanSubmitRequest{Actor: opts.Actor, Plan: string(data)}, &answer); err != nil {
			return err
		}
		rows := [][]string{{"size", orDash(answer.Size)}}
		for _, c := range answer.Children {
			rows = append(rows, []string{"child", c})
		}
		return render(cmd.OutOrStdout(), opts, answer, []string{"WHAT", "VALUE"}, rows)
	})
	planSubmit.Flags().StringVar(&planFile, "file", "", "the plan, YAML; - reads it from standard input; without it, $ISTARI_RESULT")
	plan.AddCommand(planSubmit)
	review := &cobra.Command{Use: "review", Short: "For the reviewer: submit the review of a pull request", Args: Args(cobra.NoArgs)}
	var reviewFile string
	reviewSubmit := writeCommand(opts, &cobra.Command{
		Use: "submit [--file <review.yaml>]", Short: "Submit one line per axis and the findings: axis, kind, file, line, source, quote, text", Args: Args(cobra.NoArgs),
	}, func(cmd *cobra.Command, _ []string, key string) error {
		data, err := readResult(cmd, reviewFile)
		if err != nil {
			return err
		}
		var answer api.ReviewAnswer // istari checks every finding; the verdict is code's
		if err := opts.call(cmd, "review.submit", key, api.ReviewSubmitRequest{Actor: opts.Actor, Report: string(data)}, &answer); err != nil {
			return err
		}
		rows := [][]string{{"accepted", strconv.Itoa(answer.Accepted)}, {"dropped", strconv.Itoa(answer.Dropped)}}
		return render(cmd.OutOrStdout(), opts, answer, []string{"FINDINGS", "COUNT"}, rows)
	})
	reviewSubmit.Flags().StringVar(&reviewFile, "file", "", "the review, YAML; - reads it from standard input; without it, $ISTARI_RESULT")
	review.AddCommand(reviewSubmit)
	pr := &cobra.Command{Use: "pr", Short: "For the implementer: open the pull request", Args: Args(cobra.NoArgs)}
	var prTitle, prBody string
	prOpen := writeCommand(opts, &cobra.Command{
		Use: "open --title <title> --body-file <file|->", Short: "Run the full checks and open a draft pull request linked to the ticket", Args: Args(cobra.NoArgs),
	}, func(cmd *cobra.Command, _ []string, key string) error {
		body, err := readFileOrStdin(cmd, prBody)
		if err != nil {
			return err
		}
		var opened vcs.Opened
		if err := opts.call(cmd, "pr.open", key, api.PROpenRequest{Actor: opts.Actor, Title: prTitle, Body: string(body)}, &opened); err != nil {
			return err
		}
		return render(cmd.OutOrStdout(), opts, opened, []string{"PR", "URL"}, [][]string{{fmt.Sprint(opened.Number), opened.URL}})
	})
	prOpen.Flags().StringVar(&prTitle, "title", "", "the pull request's title")
	prOpen.Flags().StringVar(&prBody, "body-file", "", "the description: Summary, Evidence, Merge Danger; - reads standard input")
	pr.AddCommand(prOpen)
	return []*cobra.Command{whoami, complete, checkpoint, refine, plan, review, pr}
}

func budgetText(w api.Whoami) string {
	if w.BudgetUSD == 0 {
		return fmt.Sprintf("$%.2f spent, no run budget", w.SpentUSD)
	}
	return fmt.Sprintf("$%.2f of $%.2f spent, $%.2f left", w.SpentUSD, w.BudgetUSD, w.BudgetLeftUSD)
}

// readResult reads a role's result: the file given, standard input for "-",
// or without --file the run's result file, $ISTARI_RESULT (spec section
// 7.6): the one file the refiner, the planner and the reviewer may write.
func readResult(cmd *cobra.Command, path string) ([]byte, error) {
	if path != "" {
		return readFileOrStdin(cmd, path)
	}
	result := os.Getenv("ISTARI_RESULT")
	if result == "" {
		return nil, usageError{fmt.Errorf("--file is required (- for standard input) outside a run that has a result file, ISTARI_RESULT")}
	}
	data, err := os.ReadFile(result)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no result in $ISTARI_RESULT (%s): write the result there first, or pass --file", result)
	}
	return data, err
}

// readFileOrStdin reads a file, or standard input for "-".
func readFileOrStdin(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "" {
		return nil, usageError{fmt.Errorf("--file is required (- for standard input)")}
	}
	if path == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(path)
}
