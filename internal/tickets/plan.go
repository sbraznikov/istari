package tickets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sbraznikov/istari/internal/core"
)

// Plan is a set of linked tickets created in one call (spec section 4.9:
// istari ticket create --file plan.yaml). Every ticket has a key; needs and
// parent name keys of the plan or existing tickets.
type Plan struct {
	Repo    string       `yaml:"repo" json:"repo,omitempty"`
	Tickets []PlanTicket `yaml:"tickets" json:"tickets"`
}

// PlanTicket is one ticket of a plan.
type PlanTicket struct {
	Key    string   `yaml:"key" json:"key"`
	Title  string   `yaml:"title" json:"title"`
	Body   string   `yaml:"body" json:"body,omitempty"`
	Labels []string `yaml:"labels" json:"labels,omitempty"`
	Needs  []string `yaml:"needs" json:"needs,omitempty"`
	Parent string   `yaml:"parent" json:"parent,omitempty"`
	// Type and Size are a split child's (spec 7.6): code writes them into the
	// table of its body.
	Type string `yaml:"type" json:"type,omitempty"`
	Size string `yaml:"size" json:"size,omitempty"`
	// Repo is the ticket's repository when it is not the plan's (spec 4.4).
	Repo string `yaml:"repo" json:"repo,omitempty"`
}

// ParsePlan reads a plan file.
func ParsePlan(data []byte) (Plan, error) {
	var p Plan
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return Plan{}, fmt.Errorf("plan: %w", err)
	}
	if len(p.Tickets) == 0 {
		return Plan{}, errors.New("plan: no tickets")
	}
	return p, nil
}

var planKey = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Creator is what creating a plan needs of a tracker.
type Creator interface {
	Get(ctx context.Context, ref Ref) (Ticket, error)
	Create(ctx context.Context, d Draft) (Ticket, error)
	Link(ctx context.Context, ref Ref, l Link) error
	SetState(ctx context.Context, ref Ref, s State) error
}

// CreatePlan validates the whole plan before it writes anything, then creates
// its tickets so that a ticket's needs and parent exist before it: keys in
// the bodies become references, the needs and the parent are written to the
// Dependencies section, and children are linked to their parent. If a write
// fails, the tickets already created are canceled: all or nothing.
func CreatePlan(ctx context.Context, t Creator, p Plan, defaultRepo string) (map[string]Ref, error) {
	repo := p.Repo
	if repo == "" {
		repo = defaultRepo
	}
	order, err := validatePlan(ctx, t, p)
	if err != nil {
		return nil, err
	}
	refs := map[string]Ref{}
	var created []Ref
	undo := func(cause error) error {
		errs := []error{cause}
		for _, r := range created {
			if err := t.SetState(context.WithoutCancel(ctx), r, State{State: core.TicketCanceled}); err != nil {
				errs = append(errs, fmt.Errorf("canceling %s: %w", r, err))
			}
		}
		return fmt.Errorf("plan: %w; the %d tickets created before were canceled", errors.Join(errs...), len(created))
	}
	for _, i := range order {
		pt := p.Tickets[i]
		in := repo // the ticket's repository
		if pt.Repo != "" {
			in = pt.Repo
		}
		written := func(name string) string { // a key or a ref, as the ticket's repository writes it
			r, ok := refs[name]
			if !ok {
				r, _ = ParseRef(name)
			}
			if r.Provider == "gh" && r.Repo == in {
				return "#" + strconv.Itoa(r.Number)
			}
			return r.String()
		}
		body := replaceKeys(pt.Body, func(key string) (string, bool) {
			if _, ok := refs[key]; ok {
				return written(key), true
			}
			return "", false
		})
		var deps []string
		if len(pt.Needs) > 0 && !strings.Contains(body, "- Needs:") {
			var needs []string
			for _, n := range pt.Needs {
				needs = append(needs, written(n))
			}
			deps = append(deps, "- Needs: "+strings.Join(needs, ", "))
		}
		if pt.Parent != "" && !strings.Contains(body, "- Parent:") {
			deps = append(deps, "- Parent: "+written(pt.Parent))
		}
		if len(deps) > 0 {
			body = withDependencies(body, deps)
		}
		tk, err := t.Create(ctx, Draft{Repo: in, Title: pt.Title, Body: body, Labels: pt.Labels})
		if err != nil {
			return nil, undo(err)
		}
		refs[pt.Key], created = tk.Ref, append(created, tk.Ref)
		if pt.Parent != "" {
			parent := refs[pt.Parent]
			if parent.Provider == "" {
				parent, _ = ParseRef(pt.Parent)
			}
			if err := t.Link(ctx, parent, Link{Kind: LinkChild, Child: tk.Ref}); err != nil {
				return nil, undo(err)
			}
		}
	}
	return refs, nil
}

// planKeyRef is a #key outside a word: not followed by a letter, digit, _ or -.
var planKeyRef = regexp.MustCompile(`#([a-z][a-z0-9-]*)([^A-Za-z0-9_-]|$)`)

// replaceKeys replaces each #key that with knows, outside fenced code blocks.
func replaceKeys(body string, with func(key string) (string, bool)) string {
	parts := strings.Split(body, "```")
	for i := 0; i < len(parts); i += 2 { // even parts are outside the fences
		parts[i] = planKeyRef.ReplaceAllStringFunc(parts[i], func(m string) string {
			sub := planKeyRef.FindStringSubmatch(m)
			if ref, ok := with(sub[1]); ok {
				return ref + sub[2]
			}
			return m
		})
	}
	return strings.Join(parts, "```")
}

// withDependencies adds lines to the body's Dependencies section, which is
// added when there is none.
func withDependencies(body string, lines []string) string {
	body = strings.TrimRight(body, "\n")
	const heading = "## Dependencies"
	if i := strings.Index(body, heading); i >= 0 {
		at := i + len(heading)
		return body[:at] + "\n\n" + strings.Join(lines, "\n") + body[at:] + "\n"
	}
	return body + "\n\n" + heading + "\n\n" + strings.Join(lines, "\n") + "\n"
}

// validatePlan checks keys, needs and parents, and returns the order to
// create the tickets in: each after what it needs and its parent.
func validatePlan(ctx context.Context, t Creator, p Plan) ([]int, error) {
	index := map[string]int{}
	for i, pt := range p.Tickets {
		switch {
		case !planKey.MatchString(pt.Key):
			return nil, fmt.Errorf("plan: ticket %d: the key %q is not lower-case letters, digits and dashes", i+1, pt.Key)
		case pt.Title == "":
			return nil, fmt.Errorf("plan: %s has no title", pt.Key)
		}
		if _, dup := index[pt.Key]; dup {
			return nil, fmt.Errorf("plan: the key %s is used twice", pt.Key)
		}
		index[pt.Key] = i
	}
	after := make([][]int, len(p.Tickets)) // ticket → the plan's tickets it comes after
	for i, pt := range p.Tickets {
		names := slices.Clone(pt.Needs)
		if pt.Parent != "" {
			names = append(names, pt.Parent)
		}
		for _, name := range names {
			if j, ok := index[name]; ok {
				after[i] = append(after[i], j)
				continue
			}
			ref, err := ParseRef(name)
			if err != nil {
				return nil, fmt.Errorf("plan: %s names %q, which is no key of the plan and no ticket", pt.Key, name)
			}
			existing, err := t.Get(ctx, ref)
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("plan: %s names %s, which does not exist", pt.Key, ref)
			}
			if err != nil {
				return nil, err
			}
			if existing.State.State == core.TicketCanceled || existing.ClosedAs == "not_planned" {
				return nil, fmt.Errorf("plan: %s names %s, which is canceled", pt.Key, ref)
			}
		}
	}
	order, err := createOrder(p, after)
	if err != nil {
		return nil, err
	}
	// A body may name only keys created before it: their refs are known then.
	pos := map[string]int{}
	for at, i := range order {
		pos[p.Tickets[i].Key] = at
	}
	for at, i := range order {
		var bad error
		replaceKeys(p.Tickets[i].Body, func(key string) (string, bool) {
			if j, ok := pos[key]; ok && j >= at && bad == nil {
				bad = fmt.Errorf("plan: %s mentions #%s, which is created after it: make it a need", p.Tickets[i].Key, key)
			}
			return "", false
		})
		if bad != nil {
			return nil, bad
		}
	}
	return order, nil
}

// createOrder orders the tickets after what they need and their parent.
func createOrder(p Plan, after [][]int) ([]int, error) {
	var order []int
	mark := make([]int, len(p.Tickets)) // 1 visiting, 2 done
	var visit func(i int, path []string) error
	visit = func(i int, path []string) error {
		switch mark[i] {
		case 1:
			return fmt.Errorf("plan: a cycle: %s", strings.Join(append(path, p.Tickets[i].Key), " → "))
		case 2:
			return nil
		}
		mark[i] = 1
		for _, j := range after[i] {
			if err := visit(j, append(path, p.Tickets[i].Key)); err != nil {
				return err
			}
		}
		mark[i] = 2
		order = append(order, i)
		return nil
	}
	for i := range p.Tickets {
		if err := visit(i, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}
