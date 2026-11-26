package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/tickets/standard"
)

// RefineResult is what the refiner submits (spec section 4.5, step 3):
// verdict, size, deviations, assumptions, and what the verdict needs.
type RefineResult struct {
	Verdict     string                 `yaml:"verdict" json:"verdict"` // meets, rewrite, needs_input or duplicate
	Size        string                 `yaml:"size" json:"size"`
	SizeReason  string                 `yaml:"size_reason" json:"size_reason,omitempty"` // why the size differs from the author's
	Deviations  []string               `yaml:"deviations" json:"deviations,omitempty"`
	Assumptions []string               `yaml:"assumptions" json:"assumptions,omitempty"`
	Body        string                 `yaml:"body" json:"body,omitempty"`       // rewrite: the new text, with the original kept
	Changes     []string               `yaml:"changes" json:"changes,omitempty"` // rewrite: what was changed
	Questions   []string               `yaml:"questions" json:"questions,omitempty"`
	Card        *journal.DecisionEntry `yaml:"card" json:"card,omitempty"` // duplicate or contradiction
}

// ParseRefine reads and validates a refiner's result; code rejects what does
// not conform (spec section 7.7).
func ParseRefine(data []byte) (RefineResult, error) {
	var r RefineResult
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return RefineResult{}, yamlError("refine result", data, err)
	}
	return r, r.validate()
}

func (r RefineResult) validate() error {
	bad := func(format string, args ...any) error { return fmt.Errorf("refine result: "+format, args...) }
	if !slices.Contains(core.Sizes(), r.Size) {
		return bad("size %q is not one of %s", r.Size, strings.Join(core.Sizes(), ", "))
	}
	switch r.Verdict {
	case "meets":
	case "rewrite":
		if r.Body == "" || len(r.Changes) == 0 {
			return bad("a rewrite needs the new body and the list of changes")
		}
	case "needs_input":
		if len(r.Questions) == 0 {
			return bad("needs_input needs the questions for the author")
		}
	case "duplicate":
		if r.Card == nil {
			return bad("a duplicate or contradiction needs a decision card")
		}
		if _, err := journal.Render(journal.Entry{Kind: journal.Decision, Actor: "refiner", Decision: r.Card}); err != nil {
			return bad("%v", err)
		}
	default:
		return bad("verdict %q is not meets, rewrite, needs_input or duplicate", r.Verdict)
	}
	return nil
}

// RefineTracker is what applying a refiner's result needs of the tracker.
type RefineTracker interface {
	Get(ctx context.Context, ref tickets.Ref) (tickets.Ticket, error)
	SetBody(ctx context.Context, ref tickets.Ref, body string) error
	SetSize(ctx context.Context, ref tickets.Ref, size string) error
	Comment(ctx context.Context, ref tickets.Ref, body string) (tickets.CommentRef, error)
}

// Refines applies refiner results (spec section 4.5): code checks them and
// does the writing.
type Refines struct {
	tracker RefineTracker
	journal *journal.Journal
	tickets *core.Tickets
	check   tickets.Standards
}

// NewRefines returns the refine applier.
func NewRefines(t RefineTracker, j *journal.Journal, k *core.Tickets, check tickets.Standards) *Refines {
	return &Refines{tracker: t, journal: j, tickets: k, check: check}
}

// Journal returns the journal the results are recorded in.
func (r *Refines) Journal() *journal.Journal { return r.journal }

// Apply applies a validated result to the run's ticket. A rewrite must keep
// the original text byte for byte in its block, or nothing is written.
func (r *Refines) Apply(ctx context.Context, run Run, res RefineResult) error {
	if err := res.validate(); err != nil {
		return err
	}
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return err
	}
	t, err := r.tracker.Get(ctx, ref)
	if err != nil {
		return err
	}
	var notes []string
	switch res.Verdict {
	case "meets":
		// The refiner runs for tickets below the standard: "meets" must hold.
		if r.check != nil {
			conf, err := r.check.Check(ctx, ref.Repo, t)
			if err != nil {
				return err
			}
			if len(conf.Deviations) > 0 {
				return fmt.Errorf("refine result: the ticket does not meet the standard (%s): rewrite it or ask", strings.Join(conf.Deviations, "; "))
			}
		}
	case "rewrite":
		original := strings.Trim(strings.ReplaceAll(journal.StripWorkState(t.Body), "\r\n", "\n"), "\n")
		if err := keptOriginal(res.Body, original); err != nil {
			return err
		}
		body := res.Body
		if journal.StripWorkState(t.Body) != t.Body { // keep the work state block
			entries, err := r.journal.Stored(ctx, ref)
			if err != nil {
				return err
			}
			body = journal.WithWorkState(body, journal.Summarize(entries))
		}
		if err := r.tracker.SetBody(ctx, ref, body); err != nil {
			return err
		}
		t.Body = body
		if r.check != nil {
			conf, err := r.check.Check(ctx, ref.Repo, t)
			if err != nil {
				return err
			}
			if len(conf.Deviations) > 0 {
				notes = append(notes, "Still not meeting the standard, so the ticket stays `new`: "+strings.Join(conf.Deviations, "; "))
			}
		}
	}
	if err := r.size(ctx, run, ref, t, res, &notes); err != nil {
		return err
	}
	// The Refined comment is the first journal write: a tracker outage on it
	// leaves no entry of this run behind for the next run to repeat (spec
	// section 4.2). Body and size label before it are set again by that run.
	var comment strings.Builder
	fmt.Fprintf(&comment, "**Refined** — %s: %s", run.Agent(), res.Verdict)
	list := func(title string, items []string) {
		if len(items) > 0 {
			comment.WriteString("\n\n" + title + ":")
			for _, it := range items {
				comment.WriteString("\n- " + it)
			}
		}
	}
	list("Changed", res.Changes)
	list("Assumptions", res.Assumptions)
	list("Deviations", res.Deviations)
	list("Notes", notes)
	if _, err := r.tracker.Comment(ctx, ref, comment.String()); err != nil {
		return err
	}
	for _, a := range res.Assumptions {
		if err := r.journal.Append(ctx, ref, journal.Entry{Kind: journal.Assumption, Actor: run.Agent(), Text: a}); err != nil {
			return err
		}
	}
	switch res.Verdict {
	case "needs_input":
		for i, q := range res.Questions {
			e := journal.Entry{Kind: journal.Question, Actor: run.Agent(), ID: fmt.Sprintf("%s-q%d", run.ID, i+1), Text: q}
			if err := r.journal.Append(ctx, ref, e); err != nil {
				return err
			}
		}
		return r.hold(ctx, run)
	case "duplicate":
		if err := r.journal.Append(ctx, ref, journal.Entry{Kind: journal.Decision, Actor: run.Agent(), Decision: res.Card}); err != nil {
			return err
		}
		return r.hold(ctx, run)
	}
	return nil
}

// size sets the size label: the refiner's, explained when it differs from
// the author's; XL is marked for a split (spec section 4.7).
func (r *Refines) size(ctx context.Context, run Run, ref tickets.Ref, t tickets.Ticket, res RefineResult, notes *[]string) error {
	var author string
	for _, l := range t.Labels {
		if s, ok := strings.CutPrefix(l, "size:"); ok && slices.Contains(core.Sizes(), s) {
			author = s
		}
	}
	if author != res.Size {
		reason := res.SizeReason
		if reason == "" {
			reason = "the refiner sized it by its scope"
		}
		from := author
		if from == "" {
			from = "none"
		}
		*notes = append(*notes, fmt.Sprintf("Size %s → %s: %s", from, res.Size, reason))
		// Every change of the size is in the journal (spec section 4.7).
		e := journal.Entry{Kind: journal.Assumption, Actor: run.Agent(), Text: fmt.Sprintf("Size %s → %s: %s", from, res.Size, reason)}
		if err := r.journal.Append(ctx, ref, e); err != nil {
			return err
		}
	}
	if res.Size == "XL" {
		*notes = append(*notes, "XL: this ticket is never executed as it is; the planner must split it into child tickets (T5)")
	}
	return r.tracker.SetSize(ctx, ref, res.Size)
}

func (r *Refines) hold(ctx context.Context, run Run) error {
	_, err := r.tickets.SetHold(ctx, run.Agent(), run.Ticket, "needs_input")
	var ce *core.Error
	if errors.As(err, &ce) && ce.Code == core.CodeTicketHeld {
		return nil
	}
	return err
}

// keptOriginal checks that body keeps the original text, byte for byte, in
// its original-text block.
func keptOriginal(body, original string) error {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	i := strings.Index(body, standard.OriginalStart)
	j := strings.LastIndex(body, standard.OriginalEnd)
	if i < 0 || j < i {
		return errors.New("refine result: the rewrite lacks the original text in a " + standard.OriginalStart + " block")
	}
	if strings.TrimSpace(body[j+len(standard.OriginalEnd):]) != "" {
		return errors.New("refine result: the original text's block must end the ticket")
	}
	if inner := strings.Trim(body[i+len(standard.OriginalStart):j], "\n"); inner != original {
		return errors.New("refine result: the original text in its block differs from the ticket's: the rewrite is rejected")
	}
	return nil
}
