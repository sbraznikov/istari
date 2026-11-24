// Package journal keeps the ticket journal (spec section 4.6): typed entries
// stored as comments on the ticket, and the "Work state" block in the ticket
// body that code builds from them. Nothing here calls a model.
package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Kind is the type of a journal entry.
type Kind string

// The entry types of spec section 4.6.
const (
	Question   Kind = "question"
	Answer     Kind = "answer"
	Decision   Kind = "decision"
	Assumption Kind = "assumption"
	Plan       Kind = "plan"
	Checkpoint Kind = "checkpoint"
	Review     Kind = "review"
	Transition Kind = "transition"
)

// Entry is one journal entry. Which fields it uses depends on its kind:
// question and answer use ID, Text and (answer) Link; assumption uses Text;
// the others their own part.
type Entry struct {
	Kind       Kind             `json:"kind"`
	At         time.Time        `json:"at"`
	Actor      string           `json:"actor"`
	ID         string           `json:"id,omitempty"`   // a question's id; its answer names it
	Text       string           `json:"text,omitempty"` // the question, the answer verbatim, the assumption
	Link       string           `json:"link,omitempty"` // where the answer was given
	Decision   *DecisionEntry   `json:"decision,omitempty"`
	Plan       *PlanEntry       `json:"plan,omitempty"`
	Checkpoint *CheckpointEntry `json:"checkpoint,omitempty"`
	Review     *ReviewEntry     `json:"review,omitempty"`
	Transition *TransitionEntry `json:"transition,omitempty"`
}

// DecisionEntry is a decision card and, once made, the choice (spec 4.6).
type DecisionEntry struct {
	Problem        string   `json:"problem"`
	State          string   `json:"state"`
	Options        []Option `json:"options"`
	Recommendation string   `json:"recommendation"` // which option and why
	Recommended    string   `json:"recommended"`    // the letter of the recommended option
	Status         string   `json:"status"`         // proposed, chosen or dismissed
	Chosen         string   `json:"chosen,omitempty"`
	ChosenBy       string   `json:"chosen_by,omitempty"`
	Comment        string   `json:"comment,omitempty"`
}

// Option is one option of a decision card.
type Option struct {
	Letter       string `json:"letter"`
	Gist         string `json:"gist"`
	Consequences string `json:"consequences,omitempty"`
	Size         string `json:"size,omitempty"`
	Risks        string `json:"risks,omitempty"`
	Changes      string `json:"changes,omitempty"`
}

// PlanEntry is the result of the plan stage.
type PlanEntry struct {
	Summary string   `json:"summary"`
	Slices  []string `json:"slices,omitempty"`
	Seams   []string `json:"seams,omitempty"`
	Zone    []string `json:"zone,omitempty"`
}

// CheckpointEntry is a meaningful step of work: what the agent says is done
// and next, and what code adds.
type CheckpointEntry struct {
	Done   string            `json:"done"`
	Next   string            `json:"next,omitempty"`
	Branch string            `json:"branch,omitempty"`
	Commit string            `json:"commit,omitempty"`
	Checks map[string]string `json:"checks,omitempty"` // check name → its result, as code ran it
	Files  []string          `json:"files,omitempty"`
	PR     string            `json:"pr,omitempty"`
}

// ReviewEntry is the result of the review stage; the details are in the PR.
type ReviewEntry struct {
	Verdict string `json:"verdict"`
	Summary string `json:"summary,omitempty"`
	PR      string `json:"pr,omitempty"`
}

// TransitionEntry is a change of the ticket's state or hold.
type TransitionEntry struct {
	From    string `json:"from,omitempty"`
	To      string `json:"to"`
	Trigger string `json:"trigger,omitempty"`
	Hold    string `json:"hold,omitempty"`
}

// validate refuses an entry that misses what its kind needs.
func (e Entry) validate() error {
	missing := func(what string) error { return fmt.Errorf("journal: a %s entry needs %s", e.Kind, what) }
	if e.Actor == "" {
		return missing("an actor")
	}
	switch e.Kind {
	case Question, Answer:
		if e.ID == "" || e.Text == "" {
			return missing("an id and a text")
		}
	case Assumption:
		if e.Text == "" {
			return missing("a text")
		}
	case Decision:
		return e.Decision.validate()
	case Plan:
		if e.Plan == nil || e.Plan.Summary == "" {
			return missing("a summary")
		}
	case Checkpoint:
		if e.Checkpoint == nil || e.Checkpoint.Done == "" {
			return missing("what is done")
		}
	case Review:
		if e.Review == nil || e.Review.Verdict == "" {
			return missing("a verdict")
		}
	case Transition:
		if e.Transition == nil || e.Transition.To == "" {
			return missing("the state it went to")
		}
	default:
		return fmt.Errorf("journal: %q is not an entry type", e.Kind)
	}
	return nil
}

// validate checks a decision card against its schema (spec sections 4.6 and
// 7.7): problem, current state, at least two complete options with sizes from
// the scale, and a recommendation and a choice that name existing options.
func (d *DecisionEntry) validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("journal: a decision card "+format, args...)
	}
	if d == nil || d.Problem == "" || d.State == "" {
		return bad("needs the problem and the current state")
	}
	if len(d.Options) < 2 {
		return bad("needs at least two options")
	}
	letters := map[string]bool{}
	for _, o := range d.Options {
		switch {
		case o.Letter == "" || letters[o.Letter]:
			return bad("needs a distinct letter for every option")
		case o.Gist == "" || o.Consequences == "" || o.Risks == "" || o.Changes == "":
			return bad("needs the gist, consequences, risks and changes of option %s", o.Letter)
		case !slices.Contains(core.Sizes(), o.Size):
			return bad("sizes option %s %q, not one of %s", o.Letter, o.Size, strings.Join(core.Sizes(), ", "))
		}
		letters[o.Letter] = true
	}
	if d.Recommendation == "" || !letters[d.Recommended] {
		return bad("needs a recommendation of one of its options, with its reasons")
	}
	switch d.Status {
	case "proposed", "dismissed":
	case "chosen":
		if !letters[d.Chosen] || d.ChosenBy == "" {
			return bad("chosen needs one of its options and who chose it")
		}
	default:
		return bad("status %q is not proposed, chosen or dismissed", d.Status)
	}
	return nil
}

// maxComment keeps a comment below the tracker's limit (GitHub: 65536).
const maxComment = 60000

// marker carries the entry, machine-readable, in its comment. JSON escapes
// '<' and '>', so the entry cannot end the HTML comment early.
var marker = regexp.MustCompile(`<!-- istari:journal (\{.*\}) -->`)

// Render returns the comment of an entry: a short text for people and the
// entry itself in a marker.
func Render(e Entry) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	out := headline(e) + "\n\n<!-- istari:journal " + string(data) + " -->"
	if len(out) > maxComment {
		return "", fmt.Errorf("journal: the %s entry is %d characters as a comment, more than %d", e.Kind, len(out), maxComment)
	}
	return out, nil
}

// Parse returns the entry of a comment; ok is false for any other comment.
func Parse(body string) (e Entry, ok bool, err error) {
	m := marker.FindStringSubmatch(body)
	if m == nil {
		return Entry{}, false, nil
	}
	if err := json.Unmarshal([]byte(m[1]), &e); err != nil {
		return Entry{}, false, fmt.Errorf("journal: a marked comment does not parse: %w", err)
	}
	return e, true, nil
}

// headline is what a person reads of an entry.
func headline(e Entry) string {
	by := " — " + e.Actor
	switch e.Kind {
	case Question:
		return "**Question** " + e.ID + by + "\n\n" + e.Text
	case Answer:
		return "**Answer** to " + e.ID + by + "\n\n" + e.Text + linkLine(e.Link)
	case Assumption:
		return "**Assumption**" + by + "\n\n" + e.Text
	case Decision:
		d := e.Decision
		var b strings.Builder
		fmt.Fprintf(&b, "**Decision** (%s)%s\n\n**Problem.** %s\n\n**Current state.** %s\n", d.Status, by, d.Problem, d.State)
		for _, o := range d.Options {
			mark := ""
			if o.Letter == d.Recommended {
				mark = " — recommended"
			}
			fmt.Fprintf(&b, "\n**Option %s** (%s)%s: %s\n- Consequences: %s\n- Risks: %s\n- Changes: %s\n", o.Letter, o.Size, mark, o.Gist, o.Consequences, o.Risks, o.Changes)
		}
		fmt.Fprintf(&b, "\n\n**Recommendation.** %s", d.Recommendation)
		if d.Chosen != "" {
			fmt.Fprintf(&b, "\n\n**Chosen:** %s by %s. %s", d.Chosen, d.ChosenBy, d.Comment)
		}
		return b.String()
	case Plan:
		return "**Plan**" + by + "\n\n" + e.Plan.Summary + bullets(e.Plan.Slices)
	case Checkpoint:
		c := e.Checkpoint
		s := "**Checkpoint**" + by + "\n\nDone: " + c.Done
		if c.Next != "" {
			s += "\nNext: " + c.Next
		}
		if c.Commit != "" {
			s += "\nCommit: `" + c.Commit + "`"
		}
		return s
	case Review:
		return "**Review**" + by + ": " + e.Review.Verdict + "\n\n" + e.Review.Summary
	case Transition:
		t := e.Transition
		s := "**Transition**" + by + ": `" + t.To + "`"
		if t.From != "" {
			s = "**Transition**" + by + ": `" + t.From + "` → `" + t.To + "`"
		}
		if t.Hold != "" {
			s += " (hold: " + t.Hold + ")"
		}
		return s
	}
	return string(e.Kind)
}

func linkLine(link string) string {
	if link == "" {
		return ""
	}
	return "\n\n" + link
}

func bullets(items []string) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString("\n- " + it)
	}
	return b.String()
}

// WorkState is the "Work state" summary (spec section 4.6).
type WorkState struct {
	Stage          string
	Hold           string
	Branch         string
	PR             string
	LastCheckpoint string
	OpenQuestions  []string
	Next           string
}

// Summarize builds the work state from the entries in order, by their
// fields alone.
func Summarize(entries []Entry) WorkState {
	var w WorkState
	var questions []Entry
	answered := map[string]bool{}
	for _, e := range entries {
		switch e.Kind {
		case Transition:
			w.Stage, w.Hold = e.Transition.To, e.Transition.Hold
		case Checkpoint:
			c := e.Checkpoint
			w.LastCheckpoint, w.Next = c.Done, c.Next
			if c.Commit != "" {
				w.LastCheckpoint += " (" + c.Commit + ")"
			}
			if c.Branch != "" {
				w.Branch = c.Branch
			}
			if c.PR != "" {
				w.PR = c.PR
			}
		case Review:
			if e.Review.PR != "" {
				w.PR = e.Review.PR
			}
		case Question:
			questions = append(questions, e)
		case Answer:
			answered[e.ID] = true
		}
	}
	for _, q := range questions {
		if !answered[q.ID] {
			w.OpenQuestions = append(w.OpenQuestions, q.Text)
		}
	}
	return w
}

const (
	blockStart = "<!-- istari:work-state:start -->"
	blockEnd   = "<!-- istari:work-state:end -->"
)

// orphan is a marker line left without its partner.
var orphan = regexp.MustCompile(`(?m)^.*(` + regexp.QuoteMeta(blockStart) + `|` + regexp.QuoteMeta(blockEnd) + `).*\n?`)

var markers = regexp.MustCompile(regexp.QuoteMeta(blockStart) + `|` + regexp.QuoteMeta(blockEnd))

// blocks returns the complete blocks of body, each a start marker whose next
// marker is an end marker, with the line break after it; and whether every
// marker in body belongs to one of them.
func blocks(body string) (found [][2]int, clean bool) {
	ms := markers.FindAllStringIndex(body, -1)
	clean = true
	for i := 0; i < len(ms); i++ {
		if body[ms[i][0]:ms[i][1]] == blockStart && i+1 < len(ms) && body[ms[i+1][0]:ms[i+1][1]] == blockEnd {
			stop := ms[i+1][1]
			if stop < len(body) && body[stop] == '\n' {
				stop++
			}
			found = append(found, [2]int{ms[i][0], stop})
			i++
			continue
		}
		clean = false
	}
	return found, clean
}

// render returns the block, markers included.
func (w WorkState) render() string {
	line := func(name, value string) string {
		if value == "" {
			value = "—"
		}
		return "| " + name + " | " + value + " |\n"
	}
	questions := strings.Join(w.OpenQuestions, "<br>")
	stage := w.Stage
	if w.Hold != "" {
		stage += " (hold: " + w.Hold + ")"
	}
	return blockStart + "\n### Work state\n\n| | |\n|---|---|\n" +
		line("Stage", stage) + line("Branch", w.Branch) + line("PR", w.PR) + line("Last checkpoint", w.LastCheckpoint) +
		line("Open questions", questions) + line("Next step", w.Next) +
		"\n_Maintained by istari from the ticket journal._\n" + blockEnd + "\n"
}

// WithWorkState returns body with its work state block replaced by w's,
// keeping everything outside the markers as it is. A body without a block
// gets one at the end. A block a human broke (a marker deleted or copied) is
// not trusted: the stray markers are dropped, the text around them stays,
// and a new block goes at the end.
func WithWorkState(body string, w WorkState) string {
	if found, clean := blocks(body); clean && len(found) > 0 {
		var b strings.Builder
		b.WriteString(body[:found[0][0]])
		b.WriteString(w.render())
		prev := found[0][1]
		for _, loc := range found[1:] { // a copy of the block: only one is kept
			b.WriteString(body[prev:loc[0]])
			prev = loc[1]
		}
		b.WriteString(body[prev:])
		return b.String()
	}
	body = StripWorkState(body)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + "\n" + w.render()
}

// StripWorkState returns body without its work state blocks and without stray
// markers: the ticket's own text, as the ticket standard reads it.
func StripWorkState(body string) string {
	found, _ := blocks(body)
	var b strings.Builder
	prev := 0
	for _, loc := range found {
		b.WriteString(body[prev:loc[0]])
		prev = loc[1]
	}
	b.WriteString(body[prev:])
	return orphan.ReplaceAllString(b.String(), "")
}

// Tracker is what the journal needs of a ticket tracker.
type Tracker interface {
	Get(ctx context.Context, ref tickets.Ref) (tickets.Ticket, error)
	Comment(ctx context.Context, ref tickets.Ref, body string) (tickets.CommentRef, error)
	Comments(ctx context.Context, ref tickets.Ref) ([]tickets.Comment, error)
	SetBody(ctx context.Context, ref tickets.Ref, body string) error
}

// EventPrefix starts the type of the event that records an entry in istari's
// log, e.g. journal.checkpoint.
const EventPrefix = "journal."

// Journal writes entries to the ticket and to istari's log, and keeps the
// work state block.
type Journal struct {
	mu      sync.Mutex // one entry at a time, so the log and the tracker agree on the order
	tracker Tracker
	store   store.Store
	clock   func() time.Time
}

// New returns a journal.
func New(t Tracker, s store.Store, clock func() time.Time) *Journal {
	if clock == nil {
		clock = time.Now
	}
	return &Journal{tracker: t, store: s, clock: clock}
}

// Append records an entry: as a comment on the ticket, in istari's log, and
// in the work state block of the ticket body.
func (j *Journal) Append(ctx context.Context, ref tickets.Ref, e Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e.At.IsZero() {
		e.At = j.clock().UTC()
	}
	body, err := Render(e)
	if err != nil {
		return err
	}
	if _, err := j.tracker.Comment(ctx, ref, body); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.store.Append(ctx, store.Event{Type: EventPrefix + string(e.Kind), At: e.At, Actor: e.Actor, Ticket: ref.String(), Payload: data}); err != nil {
		return err
	}
	entries, err := j.Stored(ctx, ref)
	if err != nil {
		return err
	}
	t, err := j.tracker.Get(ctx, ref)
	if err != nil {
		return err
	}
	if next := WithWorkState(t.Body, Summarize(entries)); next != t.Body {
		return j.tracker.SetBody(ctx, ref, next)
	}
	return nil
}

// Stored returns the ticket's journal as istari's log has it.
func (j *Journal) Stored(ctx context.Context, ref tickets.Ref) ([]Entry, error) {
	events, err := j.store.Read(ctx, "", store.Filter{Ticket: ref.String(), TypePrefix: EventPrefix}, 0)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(events))
	for _, ev := range events {
		var e Entry
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			return nil, fmt.Errorf("journal: event %s: %w", ev.Cursor, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// Read rebuilds the ticket's journal from its comments alone: istari's own
// marked comments, in order. A human's comment that imitates a marker is not
// an entry.
func (j *Journal) Read(ctx context.Context, ref tickets.Ref) ([]Entry, error) {
	comments, err := j.tracker.Comments(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	var errs []error
	for _, c := range comments {
		if !c.Own {
			continue
		}
		e, ok, err := Parse(c.Body)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, errors.Join(errs...)
}
