// Package standard checks a ticket against the structural half of the
// ticket standard (spec section 4.5): what code checks, as opposed to the
// quality the refiner judges. The standard is a repository's
// .istari/ticket-standard.toml.
package standard

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Standard is the schema of .istari/ticket-standard.toml.
type Standard struct {
	Sections     []string `toml:"sections"` // required, non-empty
	Types        []string `toml:"types"`
	Sizes        []string `toml:"sizes"`
	Priorities   []string `toml:"priorities"`
	Repository   bool     `toml:"repository"`   // the ticket names its owner/repo
	Dependencies string   `toml:"dependencies"` // the line that lists dependencies, e.g. "Needs"
}

// Default is the standard of spec section 4.5, used when a repository has
// no .istari/ticket-standard.toml.
func Default() Standard {
	return Standard{
		Sections:     []string{"Context", "What needs to be done", "Acceptance criteria", "Out of scope", "Dependencies"},
		Types:        []string{"feature", "bug", "chore", "epic"},
		Sizes:        []string{"XS", "S", "M", "L", "XL"},
		Priorities:   []string{"P0", "P1", "P2", "P3"},
		Repository:   true,
		Dependencies: "Needs",
	}
}

// Load reads a standard file; keys it leaves out keep the default.
func Load(path string) (Standard, error) {
	std := Default()
	md, err := toml.DecodeFile(path, &std)
	if err != nil {
		return Standard{}, fmt.Errorf("ticket standard %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Standard{}, fmt.Errorf("ticket standard %s: unknown key %s", path, undecoded[0])
	}
	return std, nil
}

// Draft is a ticket to check: an existing one (Ref set, labels from the
// tracker) or a file not yet created.
type Draft struct {
	Ref    *tickets.Ref
	Title  string
	Body   string
	Labels []string
}

// Deviation is one way a ticket does not meet the standard.
type Deviation struct {
	Rule    string `json:"rule"` // section, type, size, priority, repository, dependency, cycle or path
	Message string `json:"message"`
}

// Result is what the check found.
type Result struct {
	Deviations []Deviation `json:"deviations"`
	Notes      []string    `json:"notes,omitempty"` // e.g. a defaulted priority
	Type       string      `json:"type,omitempty"`
	Size       string      `json:"size,omitempty"`
	Priority   string      `json:"priority,omitempty"`
	Repository string      `json:"repository,omitempty"`
	Needs      []string    `json:"needs,omitempty"`
}

// OK tells whether the ticket meets the structural standard.
func (r Result) OK() bool { return len(r.Deviations) == 0 }

// Checker checks tickets of one repository.
type Checker struct {
	Standard        Standard
	Tracker         tickets.Provider // to look up dependencies
	Root            string           // the repository's checkout, for paths; "" skips the path check
	Repo            string           // owner/repo that #n refers to
	DefaultPriority string           // [defaults] priority
}

// Check checks a draft. It reads but never writes.
func (c Checker) Check(ctx context.Context, d Draft) (Result, error) {
	var res Result
	fail := func(rule, format string, args ...any) {
		res.Deviations = append(res.Deviations, Deviation{Rule: rule, Message: fmt.Sprintf(format, args...)})
	}
	sections := parseSections(d.Body)
	for _, name := range c.Standard.Sections {
		text, ok := sections[name]
		switch {
		case !ok:
			fail("section", "the section %q is missing", name)
		case strings.TrimSpace(text) == "":
			fail("section", "the section %q is empty", name)
		}
	}

	meta := parseMeta(d.Body)
	pick := func(field, labelPrefix string, allowed []string) string {
		label := labelValue(d.Labels, labelPrefix)
		table := meta[field]
		value := label
		if label != "" && table != "" && !strings.EqualFold(label, table) {
			fail(strings.ToLower(field), "the label %s%s and the table's %s %q disagree", labelPrefix, label, field, table)
		}
		if value == "" {
			value = table
		}
		if value != "" && !slices.Contains(allowed, value) {
			fail(strings.ToLower(field), "%s %q is not one of %s", strings.ToLower(field), value, strings.Join(allowed, ", "))
		}
		return value
	}
	res.Type = pick("Type", "type:", c.Standard.Types)
	res.Size = pick("Size", "size:", c.Standard.Sizes)
	res.Priority = pick("Priority", "priority:", c.Standard.Priorities)
	if res.Type == "" {
		fail("type", "the ticket has no type (a type: label or the table's Type)")
	}
	if res.Size == "" {
		fail("size", "the ticket has no size (a size: label or the table's Size)")
	}
	if res.Priority == "" && c.DefaultPriority != "" {
		res.Priority = c.DefaultPriority
		res.Notes = append(res.Notes, fmt.Sprintf("priority: none given, so the default %s applies", c.DefaultPriority))
	}

	repo := c.Repo
	if d.Ref != nil {
		repo = d.Ref.Repo
	}
	res.Repository = meta["Repository"]
	if c.Standard.Repository {
		switch {
		case res.Repository == "":
			fail("repository", "the repository is missing (the table's Repository column)")
		case repo != "" && !strings.EqualFold(res.Repository, repo):
			fail("repository", "the ticket names %s but belongs to %s", res.Repository, repo)
		}
	}

	if err := c.checkDependencies(ctx, d, sections, repo, &res, fail); err != nil {
		return Result{}, err
	}
	if c.Root != "" {
		// Paths in the sections' text; the metadata table names the
		// repository, which is not a path.
		var text strings.Builder
		names := slices.Sorted(maps.Keys(sections))
		slices.SortStableFunc(names, func(a, b string) int {
			ia, ib := slices.Index(c.Standard.Sections, a), slices.Index(c.Standard.Sections, b)
			if ia < 0 {
				ia = len(c.Standard.Sections)
			}
			if ib < 0 {
				ib = len(c.Standard.Sections)
			}
			return ia - ib
		})
		for _, name := range names {
			text.WriteString(sections[name])
		}
		for _, p := range paths(text.String()) {
			if _, err := os.Stat(filepath.Join(c.Root, filepath.FromSlash(p))); err != nil {
				fail("path", "`%s` does not exist in the repository (mark a file the ticket creates with (new))", p)
			}
		}
	}
	return res, nil
}

var (
	refRe    = regexp.MustCompile(`gh:[A-Za-z0-9-]+/[A-Za-z0-9._-]+#\d+|#\d+`)
	headerRe = regexp.MustCompile(`^##\s+(.+?)\s*#*\s*$`)
)

// needsOf returns the refs on the dependencies line of a body.
func (c Checker) needsOf(body, repo string) []tickets.Ref {
	prefix := "- " + c.Standard.Dependencies + ":"
	var out []tickets.Ref
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		for _, m := range refRe.FindAllString(rest, -1) {
			if strings.HasPrefix(m, "#") {
				m = "gh:" + repo + m
			}
			if ref, err := tickets.ParseRef(m); err == nil {
				out = append(out, ref)
			}
		}
	}
	return out
}

// checkDependencies checks each dependency exists and is not canceled, and
// that following dependencies never leads back: a cycle is listed.
func (c Checker) checkDependencies(ctx context.Context, d Draft, sections map[string]string, repo string, res *Result, fail func(string, string, ...any)) error {
	if c.Standard.Dependencies == "" {
		return nil
	}
	needs := c.needsOf(sections["Dependencies"], repo)
	for _, n := range needs {
		res.Needs = append(res.Needs, n.String())
	}
	if c.Tracker == nil {
		return nil
	}
	self := "this ticket"
	if d.Ref != nil {
		self = d.Ref.String()
	}
	cache := map[string]tickets.Ticket{}
	get := func(ref tickets.Ref) (tickets.Ticket, bool, error) {
		if t, ok := cache[ref.String()]; ok {
			return t, true, nil
		}
		t, err := c.Tracker.Get(ctx, ref)
		if errors.Is(err, tickets.ErrNotFound) {
			return tickets.Ticket{}, false, nil
		}
		if err != nil {
			return tickets.Ticket{}, false, err
		}
		cache[ref.String()] = t
		return t, true, nil
	}
	for _, n := range needs {
		t, found, err := get(n)
		if err != nil {
			return err
		}
		short := n.String()
		if n.Repo == repo {
			short = fmt.Sprintf("#%d", n.Number)
		}
		switch {
		case !found:
			fail("dependency", "%s does not exist", short)
		case t.State.State == core.TicketCanceled || t.ClosedAs == "not_planned":
			fail("dependency", "%s is canceled", short)
		}
	}
	// Depth-first over the dependency graph from this ticket.
	var path []string
	onPath := map[string]bool{}
	done := map[string]bool{}
	var cycle []string
	var visit func(node string, edges []tickets.Ref) error
	visit = func(node string, edges []tickets.Ref) error {
		path = append(path, node)
		onPath[node] = true
		defer func() { path = path[:len(path)-1]; onPath[node] = false; done[node] = true }()
		for _, e := range edges {
			key := e.String()
			if key == self || onPath[key] {
				start := slices.Index(path, key)
				if key == self {
					start = 0
				}
				cycle = append(slices.Clone(path[start:]), key)
				return nil
			}
			if done[key] || cycle != nil || len(done) > 500 {
				continue
			}
			t, found, err := get(e)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			if err := visit(key, c.needsOf(parseSections(t.Body)["Dependencies"], e.Repo)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(self, needs); err != nil {
		return err
	}
	if cycle != nil {
		fail("cycle", "the dependencies form a cycle: %s", strings.Join(cycle, " → "))
	}
	return nil
}

// parseSections maps each "## Name" heading to its text, up to the next
// heading or a "---" rule.
func parseSections(body string) map[string]string {
	sections := map[string]string{}
	current := ""
	var text strings.Builder
	flush := func() {
		if current != "" {
			sections[current] = text.String()
		}
		text.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		if m := headerRe.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			flush()
			current = m[1]
			continue
		}
		if strings.TrimSpace(line) == "---" {
			flush()
			current = ""
			continue
		}
		if current != "" {
			text.WriteString(line + "\n")
		}
	}
	flush()
	return sections
}

// parseMeta reads the metadata table: a header row naming Type, Size,
// Priority and Repository, a separator, and one row of values.
func parseMeta(body string) map[string]string {
	lines := strings.Split(body, "\n")
	for i := 0; i+2 < len(lines); i++ {
		header := cells(lines[i])
		if !slices.Contains(header, "Type") || !slices.Contains(header, "Size") {
			continue
		}
		values := cells(lines[i+2])
		meta := map[string]string{}
		for j, h := range header {
			if j < len(values) {
				meta[h] = strings.Trim(values[j], "` ")
			}
		}
		return meta
	}
	return map[string]string{}
}

func cells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") {
		return nil
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func labelValue(labels []string, prefix string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, prefix); ok {
			return v
		}
	}
	return ""
}

var (
	codeRe = regexp.MustCompile("`([^`\n]+)`(\\s*\\(new\\))?")
	pathRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*/?$`)
	extRe  = regexp.MustCompile(`\.[A-Za-z][A-Za-z0-9]*$`)
)

// paths returns the backticked file paths of a body that the ticket does not
// mark (new). A path has a slash or a file extension; commands, flags, refs,
// labels and globs are not paths.
func paths(body string) []string {
	var out []string
	for _, m := range codeRe.FindAllStringSubmatch(body, -1) {
		p := m[1]
		if m[2] != "" || !pathRe.MatchString(p) || strings.HasPrefix(p, "-") || strings.HasPrefix(p, ".") && !strings.Contains(p, "/") {
			continue
		}
		if !strings.Contains(p, "/") && !extRe.MatchString(p) {
			continue
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}
