package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/project"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/standard"
)

var issueNumberRe = regexp.MustCompile(`^#(\d+)$`)

// newTicketCheckCommand returns istari ticket check: the structural check of
// the ticket standard (spec section 4.5). It reads and never writes.
func newTicketCheckCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "check <ref | #number | file>",
		Short: "Show how a ticket deviates from the standard; changes nothing",
		Args:  Args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			checker := standard.Checker{Standard: standard.Default()}
			cfg, err := loadConfig(opts.Config)
			if err != nil {
				return err
			}
			checker.DefaultPriority = cfg.Defaults.Priority
			root, _ := project.Root(cwd)
			if root != "" {
				checker.Repo, _ = project.Remote(root)
			}
			var notes []string
			tracker, trackerErr := github.FromEnvironment(cmd.Context())
			if trackerErr == nil {
				checker.Tracker = tracker
			} else {
				notes = append(notes, "dependencies were not looked up: "+trackerErr.Error())
			}

			draft, err := readDraft(cmd, args[0], checker.Repo, tracker, trackerErr)
			if err != nil {
				return err
			}
			// Paths and the repository's own standard come from a checkout of
			// the ticket's repository; another repository's ticket gets the
			// default standard and no path check.
			if root != "" && (draft.Ref == nil || strings.EqualFold(draft.Ref.Repo, checker.Repo)) {
				checker.Root = root
				if path := filepath.Join(root, ".istari", "ticket-standard.toml"); fileExists(path) {
					if checker.Standard, err = standard.Load(path); err != nil {
						return err
					}
				}
			} else if draft.Ref != nil {
				notes = append(notes, fmt.Sprintf("the default standard applies: there is no checkout of %s here", draft.Ref.Repo))
			}
			res, err := checker.Check(cmd.Context(), draft)
			if err != nil {
				return err
			}
			res.Notes = append(res.Notes, notes...)
			if res.Deviations == nil {
				res.Deviations = []standard.Deviation{}
			}
			rows := make([][]string, len(res.Deviations))
			for i, d := range res.Deviations {
				rows[i] = []string{d.Rule, d.Message}
			}
			if opts.JSON {
				if err := render(cmd.OutOrStdout(), opts, res, nil, nil); err != nil {
					return err
				}
			} else {
				for _, n := range res.Notes {
					fmt.Fprintln(cmd.ErrOrStderr(), "note:", n)
				}
				if res.OK() {
					fmt.Fprintln(cmd.OutOrStdout(), "the ticket meets the standard")
				} else if err := render(cmd.OutOrStdout(), opts, res, []string{"RULE", "DEVIATION"}, rows); err != nil {
					return err
				}
			}
			if !res.OK() {
				return fmt.Errorf("%d deviation(s) from the ticket standard", len(res.Deviations))
			}
			return nil
		},
	}
}

// readDraft reads a ticket from the tracker (a ref or #number of the current
// repository) or from a file.
func readDraft(cmd *cobra.Command, arg, repo string, tracker tickets.Provider, trackerErr error) (standard.Draft, error) {
	ref, refErr := tickets.ParseRef(arg)
	if m := issueNumberRe.FindStringSubmatch(arg); m != nil && repo != "" {
		n, _ := strconv.Atoi(m[1])
		ref, refErr = tickets.Ref{Provider: "gh", Repo: repo, Number: n}, nil
	}
	if refErr == nil {
		if trackerErr != nil {
			return standard.Draft{}, trackerErr
		}
		t, err := tracker.Get(cmd.Context(), ref)
		if err != nil {
			return standard.Draft{}, err
		}
		return standard.Draft{Ref: &ref, Title: t.Title, Body: t.Body, Labels: t.Labels}, nil
	}
	data, err := os.ReadFile(arg)
	if errors.Is(err, os.ErrNotExist) {
		return standard.Draft{}, usageError{fmt.Errorf("%q is neither a ticket ref nor a file", arg)}
	}
	if err != nil {
		return standard.Draft{}, err
	}
	body := string(data)
	title := ""
	if first, rest, ok := strings.Cut(body, "\n"); ok && strings.HasPrefix(first, "# ") {
		title, body = strings.TrimSpace(strings.TrimPrefix(first, "# ")), rest
	}
	return standard.Draft{Title: title, Body: body}, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
