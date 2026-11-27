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
	Note       Kind = "note"    // something istari noticed; ID names the task it is about
	CI         Kind = "ci"      // a failed check on the pull request's head, with its output
	Comment    Kind = "comment" // a trusted member's comment on the issue while the ticket is in work
)

// FindingReply is the implementer's reply to a finding of a review: why it is
// not a flaw in this place (spec 7.6). Its name keeps clear of Reply, an
// answer to an open question.
const FindingReply Kind = "reply"

// Entry is one journal entry. Which fields it uses depends on its kind:
// question and answer use ID, Text and (answer) Link; assumption uses Text;
// note uses Text and ID, the task it is about; comment uses Text, a trusted
// member's comment verbatim, and Link; reply uses Text and Reply; the others
// their own part.
type Entry struct {
	Kind       Kind             `json:"kind"`
	At         time.Time        `json:"at"`
	Actor      string           `json:"actor"`
	ID         string           `json:"id,omitempty"`      // a question's id; its answer names it
	Text       string           `json:"text,omitempty"`    // the question, the answer verbatim, the assumption
	Link       string           `json:"link,omitempty"`    // where the answer was given
	Comment    int64            `json:"comment,omitempty"` // the tracker comment the entry was posted as, not in its marker; 0 without a tracker
	Decision   *DecisionEntry   `json:"decision,omitempty"`
	Plan       *PlanEntry       `json:"plan,omitempty"`
	Checkpoint *CheckpointEntry `json:"checkpoint,omitempty"`
	Review     *ReviewEntry     `json:"review,omitempty"`
	Reply      *ReplyEntry      `json:"reply,omitempty"`
	Transition *TransitionEntry `json:"transition,omitempty"`
	CI         *CIEntry         `json:"ci,omitempty"`
}

// CIEntry is a failed check on a pull request's head (spec 4.3): the end of
// its output, or why the output could not be read.
type CIEntry struct {
	Check  string `json:"check"`         // the failed checks' names
	Commit string `json:"commit"`        // the head commit they ran on
	URL    string `json:"url,omitempty"` // the run
	Output string `json:"output,omitempty"`
	Cut    bool   `json:"cut,omitempty"`   // Output is the end of a longer log
	Error  string `json:"error,omitempty"` // why the log could not be read
	Rerun  bool   `json:"rerun,omitempty"` // the failure is of istari's rerun of the commit: it failed too
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
	Red    bool              `json:"red,omitempty"`    // a check failed: code calls the checkpoint red, whatever the agent said
	// Failures is the end of a red check's output, by check.
	Failures map[string]string `json:"failures,omitempty"`
	Notes    []string          `json:"notes,omitempty"` // what code found, e.g. a slice without a test change
	Files    []string          `json:"files,omitempty"`
	PR       string            `json:"pr,omitempty"`
	// Main is the red check run again on the main branch's tip, to tell
	// whose failure it is (spec section 7.7).
	Main *MainCheck `json:"main,omitempty"`
}

// MainCheck is the result of a check run on a commit of the main branch.
type MainCheck struct {
	Commit string `json:"commit"`
	Check  string `json:"check"`
	Result string `json:"result"`           // green or red
	Output string `json:"output,omitempty"` // the end of a red run's output
}

// ReviewEntry is the result of the review stage; the details are in the PR.
type ReviewEntry struct {
	Verdict  string   `json:"verdict"`
	Summary  string   `json:"summary,omitempty"`
	PR       string   `json:"pr,omitempty"`
	Task     string   `json:"task,omitempty"`     // the review task the report is of
	Findings []string `json:"findings,omitempty"` // accepted by code
	Dropped  []string `json:"dropped,omitempty"`  // dropped by code, with why
}

// ReplyEntry names the finding a reply answers: the review task whose entry
// holds it and its number there, from 1.
type ReplyEntry struct {
	Review  string `json:"review"`
	Finding int    `json:"finding"`
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
	case Assumption, Note, Comment:
		if e.Text == "" {
			return missing("a text")
		}
	case CI:
		if e.CI == nil || e.CI.Check == "" || e.CI.Commit == "" {
			return missing("the check and the commit")
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
	case FindingReply:
		if e.Text == "" || e.Reply == nil || e.Reply.Review == "" || e.Reply.Finding < 1 {
			return missing("a text, the review task and the finding")
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
var marker = regexp.MustCompile(regexp.QuoteMeta(markerStart) + ` (\{.*\}) -->`)

// markerStart begins the marker: a comment that has it is istari's, or a
// copy of one of istari's.
const markerStart = "<!-- istari:journal"

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
	out := Text(e) + "\n\n" + markerStart + " " + string(data) + " -->"
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

// Text is what a person reads of an entry: its comment without the marker.
func Text(e Entry) string {
	by := " — " + e.Actor
	switch e.Kind {
	case Question:
		return "**Question** " + e.ID + by + "\n\n" + e.Text
	case Answer:
		return "**Answer** to " + e.ID + by + "\n\n" + e.Text + linkLine(e.Link)
	case Assumption:
		return "**Assumption**" + by + "\n\n" + e.Text
	case Note:
		return "**Note**" + by + "\n\n" + e.Text
	case Comment:
		return "**Comment**" + by + "\n\n" + e.Text + linkLine(e.Link)
	case CI:
		c := e.CI
		s := "**CI failure**" + by + ": " + c.Check + " on `" + c.Commit + "`"
		if c.Rerun {
			s += "; the rerun failed too"
		}
		s += linkLine(c.URL)
		if c.Error != "" {
			s += "\n\nThe log could not be read: " + c.Error
		}
		switch {
		case c.Output == "":
		case c.Cut:
			s += "\n\nThe end of the output, cut to its last part:\n\n" + fence(c.Output)
		default:
			s += "\n\nThe output:\n\n" + fence(c.Output)
		}
		return s
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
		p := e.Plan
		s := "**Plan**" + by + "\n\n" + p.Summary
		if len(p.Slices) > 0 {
			s += "\n\nSlices:" + bullets(p.Slices)
		}
		if len(p.Seams) > 0 {
			s += "\n\nSeams:" + bullets(p.Seams)
		}
		if len(p.Zone) > 0 {
			s += "\n\nChange zone:" + bullets(p.Zone)
		}
		return s
	case Checkpoint:
		c := e.Checkpoint
		s := "**Checkpoint**" + by + "\n\nDone: " + c.Done
		if c.Next != "" {
			s += "\nNext: " + c.Next
		}
		if c.Commit != "" {
			s += "\nCommit: `" + c.Commit + "`"
		}
		if len(c.Checks) > 0 {
			state := "green"
			if c.Red {
				state = "red"
			}
			s += "\nChecks (run by istari): " + state
		}
		for _, n := range c.Notes {
			s += "\n- " + n
		}
		return s
	case Review:
		s := "**Review**" + by + ": " + e.Review.Verdict + "\n\n" + e.Review.Summary
		if len(e.Review.Findings) > 0 {
			s += "\n\nFindings:" + numbered(e.Review.Findings)
		}
		if len(e.Review.Dropped) > 0 {
			s += "\n\nDropped by istari:" + bullets(e.Review.Dropped)
		}
		return s
	case FindingReply:
		return fmt.Sprintf("**Reply** to finding %d of review %s%s\n\n%s", e.Reply.Finding, e.Reply.Review, by, e.Text)
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

// fence puts text in a code block whose fence no line of it closes.
func fence(text string) string {
	f := "```"
	for strings.Contains(text, f) {
		f += "`"
	}
	return f + "\n" + strings.TrimRight(text, "\n") + "\n" + f
}

func bullets(items []string) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString("\n- " + it)
	}
	return b.String()
}

// numbered lists the items from 1, the number a reply names a finding by.
func numbered(items []string) string {
	var b strings.Builder
	for i, it := range items {
		fmt.Fprintf(&b, "\n%d. %s", i+1, it)
	}
	return b.String()
}

// LatestReview returns the last review entry of the entries; ok is false
// when they have none.
func LatestReview(entries []Entry) (r ReviewEntry, ok bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == Review {
			return *entries[i].Review, true
		}
	}
	return ReviewEntry{}, false
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
			if c.Red {
				w.LastCheckpoint += ", red"
			}
			if c.Branch != "" {
				w.Branch = c.Branch
			}
			if c.PR != "" {
				w.PR = c.PR
			}
		case Plan:
			if len(e.Plan.Slices) > 0 {
				w.Next = e.Plan.Slices[0] // until a checkpoint names the next step
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

// New returns a journal. A nil tracker keeps entries in istari's log only.
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
	return j.append(ctx, ref, e)
}

// append is Append with the journal locked.
func (j *Journal) append(ctx context.Context, ref tickets.Ref, e Entry) error {
	if e.At.IsZero() {
		e.At = j.clock().UTC()
	}
	body, err := Render(e)
	if err != nil {
		return err
	}
	if j.tracker != nil {
		posted, err := j.tracker.Comment(ctx, ref, body)
		if err != nil {
			return err
		}
		e.Comment = posted.ID
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.store.Append(ctx, store.Event{Type: EventPrefix + string(e.Kind), At: e.At, Actor: e.Actor, Ticket: ref.String(), Payload: data}); err != nil {
		return err
	}
	if j.tracker == nil {
		return nil // no tracker: istari's log only
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

// Restore puts the entries of the ticket's comments that istari's log lacks
// into the log, in order, without writing to the ticket: istari recover
// after the database was lost (spec section 4.6).
func (j *Journal) Restore(ctx context.Context, ref tickets.Ref) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := j.Read(ctx, ref)
	if err != nil {
		return err
	}
	stored, err := j.Stored(ctx, ref)
	if err != nil {
		return err
	}
	for i, e := range entries {
		if i < len(stored) {
			continue // the log has the entries up to here
		}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := j.store.Append(ctx, store.Event{Type: EventPrefix + string(e.Kind), At: e.At, Actor: e.Actor, Ticket: ref.String(), Payload: data}); err != nil {
			return err
		}
	}
	return nil
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
			e.Comment = c.ID
			out = append(out, e)
		}
	}
	return out, errors.Join(errs...)
}
