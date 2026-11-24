// Package standard checks a ticket against the structural half of the
// ticket standard (spec section 4.5): what code checks, as opposed to the
// quality the refiner judges. The standard is a repository's
// .istari/ticket-standard.toml.
package standard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	data, err := os.ReadFile(path)
	if err != nil {
		return Standard{}, fmt.Errorf("ticket standard %s: %w", path, err)
	}
	std, err := Parse(data)
	if err != nil {
		return Standard{}, fmt.Errorf("ticket standard %s: %w", path, err)
	}
	return std, nil
}

// Parse reads a standard; keys it leaves out keep the default.
func Parse(data []byte) (Standard, error) {
	std := Default()
	md, err := toml.Decode(string(data), &std)
	if err != nil {
		return Standard{}, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Standard{}, fmt.Errorf("unknown key %s", undecoded[0])
	}
	return std, nil
}

// Hash is a digest of a ticket's required sections, for spec 4.1 T15: an
// edit of those sections changes it, an edit elsewhere (other sections,
// labels) or of whitespace alone does not.
func Hash(std Standard, body string) string {
	b := parse(body)
	h := sha256.New()
	for _, name := range std.Sections {
		fmt.Fprintf(h, "%s\x00%s\x00", name, strings.Join(strings.Fields(b.sections[name]), " "))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Needs returns the tickets a ticket's text names on its dependency line of
// the default standard ("- Needs: …"), resolved against repo.
func Needs(body, repo string) []string {
	var out []string
	for _, r := range needsRefs(parse(body).prose, Default().Dependencies, repo) {
		if r.full != "" {
			out = append(out, r.full)
		}
	}
	return out
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
	b := parse(d.Body)
	for _, name := range c.Standard.Sections {
		text, ok := b.sections[name]
		switch {
		case !ok:
			fail("section", "the section %q is missing", name)
		case strings.TrimSpace(text) == "":
			fail("section", "the section %q is empty", name)
		}
	}

	// pick reads a field from its label (the tracker's truth) or the table,
	// ignoring case, and returns the standard's spelling of it.
	pick := func(field string, allowed []string) string {
		rule := strings.ToLower(field)
		label := labelValue(d.Labels, rule+":")
		table := b.meta[rule]
		if label != "" && table != "" && !strings.EqualFold(label, table) {
			fail(rule, "the label %s:%s and the table's %s %q disagree", rule, label, field, table)
		}
		value := label
		if value == "" {
			value = table
		}
		if value == "" {
			return ""
		}
		if i := slices.IndexFunc(allowed, func(a string) bool { return strings.EqualFold(a, value) }); i >= 0 {
			return allowed[i]
		}
		fail(rule, "%s %q is not one of %s", rule, value, strings.Join(allowed, ", "))
		return value
	}
	res.Type = pick("Type", c.Standard.Types)
	res.Size = pick("Size", c.Standard.Sizes)
	res.Priority = pick("Priority", c.Standard.Priorities)
	if res.Type == "" {
		fail("type", "the ticket has no type (a type: label or the table's Type)")
	}
	if res.Size == "" {
		fail("size", "the ticket has no size (a size: label or the table's Size)")
	}
	if res.Priority == "" {
		switch {
		case c.DefaultPriority == "":
			fail("priority", "the ticket has no priority and no default is configured")
		case !slices.Contains(c.Standard.Priorities, c.DefaultPriority):
			fail("priority", "the ticket has no priority, and the default %s is not one of %s", c.DefaultPriority, strings.Join(c.Standard.Priorities, ", "))
		default:
			res.Priority = c.DefaultPriority
			res.Notes = append(res.Notes, fmt.Sprintf("priority: none given, so the default %s applies", c.DefaultPriority))
		}
	}

	repo := c.Repo
	if d.Ref != nil {
		repo = d.Ref.Repo
	}
	res.Repository = b.meta["repository"]
	if c.Standard.Repository {
		switch {
		case res.Repository == "":
			fail("repository", "the repository is missing (the table's Repository column)")
		case repo == "":
			res.Notes = append(res.Notes, "repository: not compared, the current directory is no repository with an origin")
		case !strings.EqualFold(res.Repository, repo):
			fail("repository", "the ticket names %s but belongs to %s", res.Repository, repo)
		}
	}

	if err := c.checkDependencies(ctx, d, b, repo, &res, fail); err != nil {
		return Result{}, err
	}
	if c.Root == "" {
		res.Notes = append(res.Notes, "paths: not checked, there is no checkout of the ticket's repository here")
	} else {
		for _, p := range paths(b.prose, c.Root) {
			if _, err := os.Stat(filepath.Join(c.Root, filepath.FromSlash(p))); err != nil {
				fail("path", "`%s` does not exist in the repository (mark a file the ticket creates with (new))", p)
			}
		}
	}
	return res, nil
}

// maxGraph bounds how many tickets the cycle search reads.
const maxGraph = 500

// checkDependencies checks each dependency exists and is not canceled, and
// that following dependencies never leads back: a cycle is listed.
func (c Checker) checkDependencies(ctx context.Context, d Draft, b body, repo string, res *Result, fail func(string, string, ...any)) error {
	if c.Standard.Dependencies == "" {
		return nil
	}
	var needs []tickets.Ref
	for _, r := range needsRefs(b.prose, c.Standard.Dependencies, repo) {
		if r.full == "" {
			fail("dependency", "%s cannot be resolved: the ticket's repository is unknown", r.written)
			continue
		}
		ref, err := tickets.ParseRef(r.full)
		if err != nil {
			fail("dependency", "%s is not a ticket reference", r.written)
			continue
		}
		needs = append(needs, ref)
		res.Needs = append(res.Needs, ref.String())
	}
	if c.Tracker == nil {
		return nil
	}
	key := func(r tickets.Ref) string { return strings.ToLower(r.String()) }
	self := "this ticket"
	if d.Ref != nil {
		self = key(*d.Ref)
	}
	cache := map[string]tickets.Ticket{}
	get := func(ref tickets.Ref) (tickets.Ticket, bool, error) {
		if t, ok := cache[key(ref)]; ok {
			return t, true, nil
		}
		t, err := c.Tracker.Get(ctx, ref)
		if errors.Is(err, tickets.ErrNotFound) {
			return tickets.Ticket{}, false, nil
		}
		if err != nil {
			return tickets.Ticket{}, false, err
		}
		cache[key(ref)] = t
		return t, true, nil
	}
	for _, n := range needs {
		t, found, err := get(n)
		if err != nil {
			return err
		}
		short := n.String()
		if strings.EqualFold(n.Repo, repo) {
			short = fmt.Sprintf("#%d", n.Number)
		}
		switch {
		case !found:
			fail("dependency", "%s does not exist", short)
		case t.State.State == core.TicketCanceled || t.ClosedAs == "not_planned" || t.ClosedAs == "duplicate":
			fail("dependency", "%s is canceled", short)
		}
	}
	// Depth-first over the dependency graph from this ticket.
	names := map[string]string{self: self}
	if d.Ref != nil {
		names[self] = d.Ref.String()
	}
	var path []string
	onPath := map[string]bool{}
	done := map[string]bool{}
	var cycle []string
	truncated := false
	var visit func(node string, edges []tickets.Ref) error
	visit = func(node string, edges []tickets.Ref) error {
		path = append(path, node)
		onPath[node] = true
		defer func() { path = path[:len(path)-1]; onPath[node] = false; done[node] = true }()
		for _, e := range edges {
			k := key(e)
			names[k] = e.String()
			if cycle != nil {
				return nil
			}
			if onPath[k] {
				for _, n := range path[slices.Index(path, k):] {
					cycle = append(cycle, names[n])
				}
				cycle = append(cycle, names[k])
				return nil
			}
			if done[k] {
				continue
			}
			if len(done) >= maxGraph {
				truncated = true
				continue
			}
			t, found, err := get(e)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			if err := visit(k, refsOf(needsRefs(parse(t.Body).prose, c.Standard.Dependencies, e.Repo))); err != nil {
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
	if truncated {
		res.Notes = append(res.Notes, fmt.Sprintf("cycle: the search stopped after %d tickets; a cycle beyond them would not be found", maxGraph))
	}
	return nil
}

func refsOf(rs []ref) []tickets.Ref {
	var out []tickets.Ref
	for _, r := range rs {
		if ref, err := tickets.ParseRef(r.full); err == nil {
			out = append(out, ref)
		}
	}
	return out
}

func labelValue(labels []string, prefix string) string {
	for _, l := range labels {
		if len(l) > len(prefix) && strings.EqualFold(l[:len(prefix)], prefix) {
			return l[len(prefix):]
		}
	}
	return ""
}
