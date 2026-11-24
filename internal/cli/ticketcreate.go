package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// newTicketCreateCommand returns istari ticket create --file (spec section
// 4.9): a ticket from a Markdown file whose first line is "# <title>", or a
// set of linked tickets from a plan.yaml.
func newTicketCreateCommand(opts *Options) *cobra.Command {
	var file, repo string
	cmd := writeCommand(opts, &cobra.Command{
		Use:   "create --file <ticket.md|plan.yaml>",
		Short: "Create a ticket, or a set of linked tickets from a plan file",
		Args:  Args(cobra.NoArgs),
	}, func(cmd *cobra.Command, _ []string, key string) error {
		if file == "" {
			return usageError{fmt.Errorf("--file is required")}
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var plan tickets.Plan
		switch strings.ToLower(filepath.Ext(file)) {
		case ".yaml", ".yml":
			if plan, err = tickets.ParsePlan(data); err != nil {
				return err
			}
		default:
			title, body, _ := strings.Cut(string(data), "\n")
			title = strings.TrimSpace(strings.TrimPrefix(title, "# "))
			if title == "" {
				return fmt.Errorf("%s: the first line is the ticket's title: # <title>", file)
			}
			plan = tickets.Plan{Tickets: []tickets.PlanTicket{{Key: "ticket", Title: title, Body: strings.TrimLeft(body, "\n")}}}
		}
		if repo != "" {
			plan.Repo = repo
		}
		var refs map[string]string
		if err := opts.call(cmd, "ticket.plan", key, api.TicketPlanRequest{Actor: opts.Actor, Plan: plan}, &refs); err != nil {
			return err
		}
		var rows [][]string
		for _, pt := range plan.Tickets {
			rows = append(rows, []string{pt.Key, refs[pt.Key], pt.Title})
		}
		return render(cmd.OutOrStdout(), opts, refs, []string{"KEY", "TICKET", "TITLE"}, rows)
	})
	cmd.Flags().StringVar(&file, "file", "", "a ticket (.md) or a plan of tickets (.yaml)")
	cmd.Flags().StringVar(&repo, "repo", "", "the repository, owner/repo; default: the plan's, else the first configured")
	return cmd
}

// newTicketDepsCommand returns istari ticket deps [ref]: the dependency graph
// with the state of each ticket.
func newTicketDepsCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use: "deps [ref]", Short: "Print the dependency tree of a ticket, or of every ticket", Args: Args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var all []core.Ticket
			if err := opts.call(cmd, "ticket.list", "", api.Empty{}, &all); err != nil {
				return err
			}
			byRef := map[string]core.Ticket{}
			needed := map[string]bool{}
			for _, t := range all {
				byRef[t.Ref] = t
				for _, n := range t.Needs {
					needed[n] = true
				}
			}
			var roots []string
			if len(args) == 1 {
				if _, ok := byRef[args[0]]; !ok {
					return refusal{api.ErrorBody{Code: core.CodeNotFound, Message: "ticket " + args[0] + " does not exist"}}
				}
				roots = []string{args[0]}
			} else {
				for _, t := range all {
					if !needed[t.Ref] && len(t.Needs) > 0 {
						roots = append(roots, t.Ref)
					}
				}
			}
			slices.Sort(roots)
			// The tickets reached from the roots; a cycle that no root reaches
			// is printed from its first ticket.
			reached := map[string]bool{}
			var reach func(string)
			reach = func(ref string) {
				if reached[ref] {
					return
				}
				reached[ref] = true
				for _, n := range byRef[ref].Needs {
					reach(n)
				}
			}
			for _, r := range roots {
				reach(r)
			}
			if len(args) == 0 {
				refs := make([]string, 0, len(all))
				for _, t := range all {
					refs = append(refs, t.Ref)
				}
				slices.Sort(refs)
				for _, ref := range refs {
					if !reached[ref] && len(byRef[ref].Needs) > 0 {
						roots = append(roots, ref)
						reach(ref)
					}
				}
			}
			if opts.JSON {
				type node struct {
					Ref, State string
					Needs      []string
				}
				var out []node
				for _, t := range all {
					if reached[t.Ref] {
						out = append(out, node{t.Ref, string(t.State), t.Needs})
					}
				}
				return render(cmd.OutOrStdout(), opts, out, nil, nil)
			}
			var b strings.Builder
			var walk func(ref, indent string, seen map[string]bool)
			walk = func(ref, indent string, seen map[string]bool) {
				t, ok := byRef[ref]
				state := "unknown"
				if ok {
					state = string(t.State)
					if t.Hold != "" {
						state += " (hold: " + t.Hold + ")"
					}
				}
				if seen[ref] {
					fmt.Fprintf(&b, "%s%s  (cycle)\n", indent, ref)
					return
				}
				fmt.Fprintf(&b, "%s%s  %s  %s\n", indent, ref, state, t.Title)
				seen[ref] = true
				for _, n := range t.Needs {
					walk(n, indent+"  needs ", seen)
				}
				delete(seen, ref)
			}
			for _, r := range roots {
				walk(r, "", map[string]bool{})
			}
			if b.Len() == 0 {
				b.WriteString("no ticket has dependencies\n")
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), b.String())
			return err
		},
	}
}
