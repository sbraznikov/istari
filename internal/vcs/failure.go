package vcs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Failure is what istari read of a commit's failed checks (spec 4.3): their
// names, the commit, a link to the run and the end of their logs.
type Failure struct {
	Check  string // the failed checks' names
	Commit string
	URL    string // the first failed check's run
	Output string
	Cut    bool // Output is the end of longer logs
}

// FailureLog reads the logs of a commit's failed check runs through istari's
// own access: each run's Actions job log. An error says why no log could be
// read.
func (c *Client) FailureLog(ctx context.Context, repo, sha string) (Failure, error) {
	runs, err := c.checkRuns(ctx, repo, sha)
	if err != nil {
		return Failure{}, err
	}
	f := Failure{Commit: sha}
	var names, parts []string
	var errs []error
	for _, r := range runs {
		if r.state() != "failure" {
			continue
		}
		names = append(names, r.Name)
		if f.URL == "" {
			f.URL = r.URL
		}
		log, err := c.send(ctx, repo, http.MethodGet, fmt.Sprintf("%s/repos/%s/actions/jobs/%d/logs", c.base, repo, r.ID), nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
			continue
		}
		parts = append(parts, string(log))
	}
	f.Check = strings.Join(names, ", ")
	f.Output = strings.Join(parts, "\n")
	return f, errors.Join(errs...)
}
