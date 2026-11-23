package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/store"
)

var eventHeader = []string{"CURSOR", "TYPE", "AT", "ACTOR", "TICKET", "TASK"}

func eventRow(e store.Event) []string {
	return []string{string(e.Cursor), e.Type, timeOrDash(e.At), orDash(e.Actor), orDash(e.Ticket), orDash(e.Task)}
}

func newTailCommand(opts *Options) *cobra.Command {
	var f store.Filter
	var after string
	var follow bool
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Print the events after a cursor; --follow keeps printing new ones",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient(opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if follow {
				if !opts.JSON {
					fmt.Fprintln(out, strings.Join(eventHeader, "\t"))
				}
				return c.stream(cmd.Context(), store.Cursor(after), f, func(e store.Event) bool {
					if opts.JSON {
						print(out, opts, e, nil, nil)
					} else {
						fmt.Fprintln(out, strings.Join(eventRow(e), "\t"))
					}
					return true
				})
			}
			var all []store.Event
			cursor := store.Cursor(after)
			for {
				page, err := c.events(cmd.Context(), cursor, f)
				if err != nil {
					return err
				}
				all = append(all, page...)
				if len(page) == 0 {
					break
				}
				cursor = page[len(page)-1].Cursor
			}
			if all == nil {
				all = []store.Event{}
			}
			rows := make([][]string, len(all))
			for i, e := range all {
				rows[i] = eventRow(e)
			}
			return print(out, opts, all, eventHeader, rows)
		},
	}
	addFilterFlags(cmd, &f)
	cmd.Flags().StringVar(&after, "after", "", "print only events after this cursor")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new events")
	return cmd
}

func addFilterFlags(cmd *cobra.Command, f *store.Filter) {
	cmd.Flags().StringVar(&f.Project, "project", "", "only events of this project")
	cmd.Flags().StringVar(&f.Ticket, "ticket", "", "only events of this ticket")
	cmd.Flags().StringVar(&f.TypePrefix, "type", "", "only events whose type starts with this, e.g. task.")
}

// filterKeys are the keys of a wait filter.
var filterKeys = []string{"type", "project", "ticket", "task"}

func newWaitCommand(opts *Options) *cobra.Command {
	var timeout time.Duration
	var after string
	cmd := &cobra.Command{
		Use:   "wait <key=value>... [--timeout <duration>]",
		Short: "Wait for the first event matching type=, project=, ticket=, task= and print it; exit 1 on timeout",
		Args:  Args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var f store.Filter
			var task string
			for _, arg := range args {
				key, value, ok := strings.Cut(arg, "=")
				if !ok || !slices.Contains(filterKeys, key) || value == "" {
					return usageError{fmt.Errorf("filter %q is not key=value with a key of %s", arg, strings.Join(filterKeys, ", "))}
				}
				switch key {
				case "type":
					f.TypePrefix = value
				case "project":
					f.Project = value
				case "ticket":
					f.Ticket = value
				case "task":
					task = value
				}
			}
			c, err := newClient(opts)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			cursor := store.Cursor(after)
			if cursor == "" {
				// Wait for what happens from now on: start at the end of the log.
				if cursor, err = c.lastCursor(ctx); err != nil {
					return err
				}
			}
			var found *store.Event
			err = c.stream(ctx, cursor, f, func(e store.Event) bool {
				if task != "" && e.Task != task {
					return true
				}
				found = &e
				return false
			})
			if found != nil {
				return print(cmd.OutOrStdout(), opts, found, eventHeader, [][]string{eventRow(*found)})
			}
			if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				return fmt.Errorf("timeout: no matching event within %s", timeout)
			}
			return err
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (exit 1); 0 waits forever")
	cmd.Flags().StringVar(&after, "after", "", "consider events after this cursor (default: from now on)")
	return cmd
}

// lastCursor returns the cursor of the last event in the log.
func (c *client) lastCursor(ctx context.Context) (store.Cursor, error) {
	var cursor store.Cursor
	for {
		page, err := c.events(ctx, cursor, store.Filter{})
		if err != nil {
			return "", err
		}
		if len(page) == 0 {
			return cursor, nil
		}
		cursor = page[len(page)-1].Cursor
	}
}
