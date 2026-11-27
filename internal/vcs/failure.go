package vcs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// MaxFailureLog bounds the failure output a journal entry holds, in bytes:
// enough for the failing tests and their messages at a log's end, small
// enough for the brief (runner.MaxBrief).
const MaxFailureLog = 8000

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
	runs, err := c.failedRuns(ctx, repo, sha)
	if err != nil {
		return Failure{}, err
	}
	f := Failure{Commit: sha}
	var names, parts []string
	var errs []error
	for _, r := range runs {
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
	if len(names) == 0 {
		return f, errors.New("vcs: no check run failed: the failure is a commit status, which has no log istari can read")
	}
	f.Check = strings.Join(names, ", ")
	share := MaxFailureLog / max(1, len(parts)) // each failed job keeps the end of its own log
	for i, p := range parts {
		var cut bool
		if parts[i], cut = tail(p, share-1); cut {
			f.Cut = true
		}
	}
	f.Output = strings.Join(parts, "\n")
	return f, errors.Join(errs...)
}

// RerunFailed reruns the Actions job of each failed check run of a commit
// through istari's own access (spec 4.3) and returns the names of the jobs
// it reran. An error names each job GitHub refused to rerun.
func (c *Client) RerunFailed(ctx context.Context, repo, sha string) ([]string, error) {
	runs, err := c.failedRuns(ctx, repo, sha)
	if err != nil {
		return nil, err
	}
	var jobs []string
	var errs []error
	for _, r := range runs {
		if _, err := c.send(ctx, repo, http.MethodPost, fmt.Sprintf("%s/repos/%s/actions/jobs/%d/rerun", c.base, repo, r.ID), nil); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
			continue
		}
		jobs = append(jobs, r.Name)
	}
	return jobs, errors.Join(errs...)
}

// failedRuns lists the failed check runs of a commit.
func (c *Client) failedRuns(ctx context.Context, repo, sha string) ([]checkRun, error) {
	runs, err := c.checkRuns(ctx, repo, sha)
	if err != nil {
		return nil, err
	}
	var failed []checkRun
	for _, r := range runs {
		if r.state() == "failure" {
			failed = append(failed, r)
		}
	}
	return failed, nil
}

// tail is the end of text in at most limit bytes, starting at a line when
// one starts in it; cut tells whether anything was left out.
func tail(text string, limit int) (end string, cut bool) {
	if len(text) <= limit {
		return text, false
	}
	end = text[len(text)-limit:]
	if i := strings.IndexByte(end, '\n'); i >= 0 && i+1 < len(end) {
		return end[i+1:], true
	}
	for len(end) > 0 && !utf8.RuneStart(end[0]) {
		end = end[1:]
	}
	return end, true
}
