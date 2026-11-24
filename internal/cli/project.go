package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/project"
)

// configPath is the installation config the project commands edit.
func configPath(opts *Options) (string, error) {
	if opts.Config != "" {
		return opts.Config, nil
	}
	return config.DefaultPath()
}

// loadForEdit loads the config at path; a missing file is an empty one.
func loadForEdit(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		def := config.Default()
		return &def, nil
	}
	return cfg, err
}

func newInitCommand(opts *Options) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Make the current repository a project: register it and create .istari/",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root, err := project.Root(cwd)
			if err != nil {
				return err
			}
			repo, err := project.Remote(root)
			if err != nil {
				return err
			}
			cfgPath, err := configPath(opts)
			if err != nil {
				return err
			}
			cfg, err := loadForEdit(cfgPath)
			if err != nil {
				return err
			}
			// The files first: a failure leaves no half-registered project.
			created, err := project.Scaffold(root)
			if err != nil {
				return err
			}
			res := struct {
				Project    string   `json:"project"`
				Repo       string   `json:"repo"`
				Registered bool     `json:"registered"`
				Created    []string `json:"created"`
			}{Repo: repo, Created: created}
			if p, ok := project.ByRepo(cfg, repo); ok {
				res.Project = p.Name
			} else {
				if name == "" {
					name = path.Base(repo)
				}
				for _, p := range cfg.Projects {
					if p.Name == name {
						return usageError{fmt.Errorf("a project named %q exists; choose another name with --name", name)}
					}
				}
				if err := config.AppendProject(cfgPath, name, repo); err != nil {
					return err
				}
				res.Project, res.Registered = name, true
			}
			if opts.JSON {
				return render(cmd.OutOrStdout(), opts, res, nil, nil)
			}
			out := cmd.OutOrStdout()
			if res.Registered {
				fmt.Fprintf(out, "registered project %s (%s) in %s\n", res.Project, repo, cfgPath)
			} else {
				fmt.Fprintf(out, "%s is already part of project %s\n", repo, res.Project)
			}
			for _, f := range created {
				fmt.Fprintf(out, "created %s\n", f)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "project name (default: the repository name)")
	return cmd
}

// currentProject resolves the project of the repository the command runs in.
func currentProject(opts *Options) (config.Project, string, error) {
	cfgPath, err := configPath(opts)
	if err != nil {
		return config.Project{}, "", err
	}
	cfg, err := loadForEdit(cfgPath)
	if err != nil {
		return config.Project{}, "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return config.Project{}, "", err
	}
	p, err := project.Resolve(cfg, cwd)
	return p, cfgPath, err
}

type repoRow struct {
	Repo     string `json:"repo"`
	Primary  bool   `json:"primary"`
	DoneWhen string `json:"done_when"`
	Link     string `json:"link,omitempty"`
	Soft     bool   `json:"soft"` // whether soft dependencies are available
}

func newRepoCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "The repositories of the current project", Args: Args(cobra.NoArgs)}

	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List the project's repositories", Args: Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, _, err := currentProject(opts)
			if err != nil {
				return err
			}
			var rows []repoRow
			var table [][]string
			for _, r := range p.RepoList() {
				row := repoRow{Repo: r.Repo, Primary: r.Primary, DoneWhen: r.DoneWhen, Link: r.Link,
					Soft: project.Proposal{Link: project.Link(r.Link)}.SoftAvailable()}
				if row.DoneWhen == "" {
					row.DoneWhen = "merged"
				}
				rows = append(rows, row)
				table = append(table, []string{p.Name, row.Repo, strconv.FormatBool(row.Primary), row.DoneWhen, orDash(row.Link), strconv.FormatBool(row.Soft)})
			}
			return render(cmd.OutOrStdout(), opts, map[string]any{"project": p.Name, "repos": rows},
				[]string{"PROJECT", "REPO", "PRIMARY", "DONE WHEN", "LINK", "SOFT"}, table)
		},
	})

	var doneWhen string
	var yes bool
	add := &cobra.Command{
		Use:   "add <path to a checkout of the repository>",
		Short: "Add a repository to the project; istari proposes done_when from how the two are linked",
		Args:  Args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if doneWhen != "" && doneWhen != "merged" && doneWhen != "released" {
				return usageError{fmt.Errorf("--done-when must be merged or released, not %q", doneWhen)}
			}
			p, cfgPath, err := currentProject(opts)
			if err != nil {
				return err
			}
			cwd, _ := os.Getwd()
			here, err := project.Root(cwd)
			if err != nil {
				return err
			}
			hereRepo, err := project.Remote(here)
			if err != nil {
				return err
			}
			primary := p.RepoList()[0]
			for _, r := range p.RepoList() {
				if r.Primary {
					primary = r
				}
			}
			if !strings.EqualFold(hereRepo, primary.Repo) {
				return fmt.Errorf("run repo add from the primary repository %s: the link is measured against it", primary.Repo)
			}
			addedRoot, err := project.Root(args[0])
			if err != nil {
				return err
			}
			repo, err := project.Remote(addedRoot)
			if err != nil {
				return err
			}
			cfg, err := loadForEdit(cfgPath)
			if err != nil {
				return err
			}
			if other, ok := project.ByRepo(cfg, repo); ok {
				if other.Name == p.Name {
					fmt.Fprintf(cmd.OutOrStdout(), "%s is already part of project %s\n", repo, p.Name)
					return nil
				}
				return fmt.Errorf("%s already belongs to project %s", repo, other.Name)
			}
			proposal := project.DetectLink(here, addedRoot, primary.Repo, repo)
			out := cmd.OutOrStdout()
			if opts.JSON {
				out = cmd.ErrOrStderr() // the proposal is for the human; stdout carries the result
			}
			fmt.Fprintf(out, "%s: link %s", repo, proposal.Link)
			if proposal.Evidence != "" {
				fmt.Fprintf(out, " (%s)", proposal.Evidence)
			}
			fmt.Fprintf(out, "\nproposed: done_when = %q\n", proposal.DoneWhen)
			if !proposal.SoftAvailable() {
				fmt.Fprintln(out, "the link could not be determined: soft dependencies stay unavailable until a human describes it")
			}
			choice := proposal.DoneWhen
			switch {
			case doneWhen != "":
				choice = doneWhen
			case yes:
			case isTerminal(os.Stdin):
				if !confirm(cmd, "accept? [Y/n] ") {
					return errors.New("not added")
				}
			default:
				fmt.Fprintln(out, "confirm with --yes, or choose with --done-when merged|released")
				return errors.New("not added: a human has to confirm the proposal")
			}
			added := config.ProjectRepo{Repo: repo, DoneWhen: choice, Link: string(proposal.Link)}
			if err := config.AddRepo(cfgPath, p.Name, added); err != nil {
				return err
			}
			if opts.JSON {
				return render(cmd.OutOrStdout(), opts, map[string]any{"project": p.Name, "added": repoRow{Repo: repo, DoneWhen: choice, Link: added.Link, Soft: proposal.SoftAvailable()}}, nil, nil)
			}
			fmt.Fprintf(out, "added %s to project %s with done_when = %q\n", repo, p.Name, choice)
			return nil
		},
	}
	add.Flags().StringVar(&doneWhen, "done-when", "", "merged or released, instead of the proposal")
	add.Flags().BoolVarP(&yes, "yes", "y", false, "accept the proposal")
	cmd.AddCommand(add)

	cmd.AddCommand(&cobra.Command{
		Use: "remove <owner/repo>", Short: "Remove a repository from the project", Args: Args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfgPath, err := currentProject(opts)
			if err != nil {
				return err
			}
			name := ""
			for _, r := range p.RepoList() {
				if strings.EqualFold(r.Repo, args[0]) {
					if r.Primary {
						return fmt.Errorf("%s is the project's primary repository and cannot be removed", r.Repo)
					}
					name = r.Repo
				}
			}
			if name == "" {
				return fmt.Errorf("project %s has no repository %s", p.Name, args[0])
			}
			if err := config.RemoveRepo(cfgPath, p.Name, name); err != nil {
				return err
			}
			if opts.JSON {
				return render(cmd.OutOrStdout(), opts, map[string]string{"project": p.Name, "removed": name}, nil, nil)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s from project %s\n", name, p.Name)
			return nil
		},
	})
	return cmd
}

// isTerminal tells whether f is a terminal a human types into. /dev/null is
// a character device too, but nobody answers there.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(info, null)
}

// confirm asks a yes/no question; only an answered yes (or Enter) counts,
// never the end of the input.
func confirm(cmd *cobra.Command, question string) bool {
	fmt.Fprint(cmd.OutOrStdout(), question)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "" || answer == "y" || answer == "yes"
}
