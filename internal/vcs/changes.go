package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Changes is what a pull request changes, as auto mode weighs it (spec
// section 4.8).
type Changes struct {
	// Paths are the changed files; a renamed file names its old path too.
	Paths []string
	// Files are the changed files counted once, a rename as one.
	Files int
	// Lines are the changed lines: additions plus deletions.
	Lines int
	// MergeDanger is the text of the description's Merge Danger section, ""
	// without one.
	MergeDanger string
}

// Changes reads the pull request's changed files, over every page, and its
// description's Merge Danger section.
func (c *Client) Changes(ctx context.Context, repo string, n int) (Changes, error) {
	var pr struct {
		Body string `json:"body"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return Changes{}, err
	}
	out := Changes{MergeDanger: section(pr.Body, "Merge Danger")}
	next := fmt.Sprintf("%s/repos/%s/pulls/%d/files?per_page=100", c.base, repo, n)
	for next != "" {
		data, header, err := c.sendAs(ctx, repo, http.MethodGet, next, "application/vnd.github+json", nil)
		if err != nil {
			return Changes{}, err
		}
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
			Additions        int    `json:"additions"`
			Deletions        int    `json:"deletions"`
		}
		if err := json.Unmarshal(data, &files); err != nil {
			return Changes{}, fmt.Errorf("github: files of pull request %d: %w", n, err)
		}
		for _, f := range files {
			out.Paths = append(out.Paths, f.Filename)
			if f.PreviousFilename != "" {
				out.Paths = append(out.Paths, f.PreviousFilename)
			}
			out.Files++
			out.Lines += f.Additions + f.Deletions
		}
		next = ""
		if m := nextLink.FindStringSubmatch(header.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return out, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// section returns the text under the Markdown heading named title, up to the
// next heading; "" when there is no such heading.
func section(body, title string) string {
	var b strings.Builder
	in := false
	for line := range strings.Lines(body) {
		if heading, ok := strings.CutPrefix(strings.TrimSpace(line), "#"); ok {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(strings.TrimLeft(heading, "#")), title)
			continue
		}
		if in {
			b.WriteString(line)
		}
	}
	return strings.TrimSpace(b.String())
}
