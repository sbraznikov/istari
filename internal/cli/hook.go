package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/runner"
)

// newHookCommand returns istari hook, the commands harnesses run as hooks
// around the agent's tool calls.
func newHookCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "hook", Short: "Hooks a harness runs around the agent's tool calls", Hidden: true, Args: Args(cobra.NoArgs)}
	cmd.AddCommand(&cobra.Command{
		Use:   "git-guard",
		Short: "Deny shell commands whose git reaches beyond the run's worktree (exit 2)",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			deny := func(why string) error {
				fmt.Fprintln(cmd.ErrOrStderr(), "istari: "+why)
				os.Exit(2) // the harness's code for a denied tool call
				return nil
			}
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return deny("the hook input could not be read: " + err.Error())
			}
			var in struct {
				Tool  string `json:"tool_name"`
				Cwd   string `json:"cwd"`
				Input struct {
					Command string `json:"command"`
				} `json:"tool_input"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return deny("the hook input is not JSON: " + err.Error())
			}
			if in.Tool != "Bash" {
				return nil
			}
			cwd := in.Cwd
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			if err := runner.GitGuard(in.Input.Command, os.Getenv("ISTARI_WORKTREE"), cwd); err != nil {
				return deny(err.Error())
			}
			return nil
		},
	})
	return cmd
}
