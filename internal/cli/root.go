// Package cli holds the istari command tree.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Exit codes from spec section 9.
const (
	ExitOK      = 0 // success
	ExitFailure = 1 // refusal, failure or timeout
	ExitUsage   = 2 // usage error
)

// Version is the build version; set with -ldflags "-X github.com/sbraznikov/istari/internal/cli.Version=...".
var Version = "dev"

// Options holds the global flags shared by every command.
type Options struct {
	JSON   bool   // print machine-readable JSON instead of a table
	Config string // config file; empty means ~/.config/istari/config.toml
}

// usageError marks an error caused by how the command was invoked.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// NewRoot builds the root command.
func NewRoot(opts *Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "istari",
		Short:         "istari takes a ticket to a merged pull request",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("istari {{.Version}}\n")
	root.PersistentFlags().BoolVar(&opts.JSON, "json", false, "print JSON instead of a table")
	root.PersistentFlags().StringVar(&opts.Config, "config", "", "config file (default ~/.config/istari/config.toml)")
	root.AddCommand(newDaemonCommand(opts))
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageError{err}
	})
	return root
}

// Execute runs the command line and returns the process exit code.
func Execute(args []string, stdout, stderr io.Writer) int {
	var opts Options
	root := NewRoot(&opts)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	_, err := root.ExecuteC()
	if err == nil {
		return ExitOK
	}
	fmt.Fprintln(stderr, "istari:", err)
	if errors.As(err, new(usageError)) {
		return ExitUsage
	}
	return ExitFailure
}

// Args wraps a positional-argument validator so that its errors exit with ExitUsage.
// Every command sets Args through it; on the root this also turns an unknown
// subcommand into a usage error.
func Args(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}
