package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/harness"
	"github.com/sbraznikov/istari/internal/runner"
)

// denied is a tool call a hook refuses: istari exits with ExitDenied and
// the reason goes to the agent.
type denied struct{ error }

// ExitDenied is the exit code with which a harness hook denies a tool call.
const ExitDenied = 2

// newHookCommand returns istari hook, the commands harnesses run as hooks
// around the agent's tool calls.
func newHookCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "hook", Short: "Hooks a harness runs around the agent's tool calls", Hidden: true, Args: Args(cobra.NoArgs)}
	var name string
	guard := &cobra.Command{
		Use:   "git-guard",
		Short: "Deny the agent's tool calls that reach beyond its worktree (the harness's hook input on stdin; exit 2 denies)",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := harness.Get(name)
			if err != nil {
				return denied{err}
			}
			reader, ok := h.(harness.HookReader)
			if !ok {
				return denied{errors.New("harness " + name + " has no hooks")}
			}
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return denied{err}
			}
			call, err := reader.ReadHook(raw)
			if err != nil {
				return denied{err}
			}
			worktree := os.Getenv("ISTARI_WORKTREE")
			switch {
			case call.Command != "":
				cwd := call.Cwd
				if cwd == "" {
					cwd, _ = os.Getwd()
				}
				err = runner.GitGuard(call.Command, worktree, cwd)
			case call.Path != "":
				path := call.Path
				if !filepath.IsAbs(path) {
					path = filepath.Join(call.Cwd, path)
				}
				err = runner.FileGuard(path)
			}
			if err != nil {
				return denied{err}
			}
			return nil
		},
	}
	guard.Flags().StringVar(&name, "harness", "", "the harness whose hook input is read")
	cmd.AddCommand(guard)
	return cmd
}

func isDenied(err error) bool { return errors.As(err, new(denied)) }
