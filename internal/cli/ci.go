package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/vcs"
)

// newCICommand groups the commands a repository's CI runs.
func newCICommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ci", Short: "Checks for a repository's CI", Args: Args(cobra.NoArgs)}
	var repo string
	var pr int
	link := &cobra.Command{
		Use:   "ticket-link --repo <owner/repo> --pr <number>",
		Short: "The istari/ticket-link check: fail a pull request that references no open ticket",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" || pr <= 0 {
				return fmt.Errorf("--repo and --pr are required")
			}
			gh, err := ciGitHub(cmd.Context())
			if err != nil {
				return err
			}
			pull, err := gh.PullRequest(cmd.Context(), repo, pr)
			if err != nil {
				return err
			}
			p := vcs.PullRequest{Repo: repo, HeadRepo: pull.HeadRepo, Branch: pull.Branch, Messages: pull.Messages}
			if err := vcs.CheckTicketLink(cmd.Context(), p, gh); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %s#%d references open tickets: %s\n", repo, pr, vcs.TicketRefsText(p))
			return nil
		},
	}
	link.Flags().StringVar(&repo, "repo", "", "the repository, owner/repo")
	link.Flags().IntVar(&pr, "pr", 0, "the pull request number")
	cmd.AddCommand(link)
	return cmd
}

// ciGitHub uses GITHUB_TOKEN, which GitHub Actions provides, and the GitHub
// CLI's token otherwise.
func ciGitHub(ctx context.Context) (*github.Provider, error) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return github.New(github.Options{BaseURL: os.Getenv("GITHUB_API_URL"), Token: token}), nil
	}
	return github.FromEnvironment(ctx)
}
