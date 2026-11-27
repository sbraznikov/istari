package journal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Reply is an answer to one of a ticket's open questions (spec section 4.6).
type Reply struct {
	// Question is the id of the question it answers. Empty: the open
	// question whose id the text names, else the oldest open one.
	Question string
	Actor    string // who answered
	Text     string // the answer verbatim
	Link     string // where it was given
}

// ErrNoOpenQuestion is the answer to a ticket that has no open question.
var ErrNoOpenQuestion = errors.New("journal: the ticket has no open question")

// Answer records r as the answer entry of one of the ticket's open
// questions and returns the entry. The answer to the last open question
// lifts a needs_input hold in holds.
func (j *Journal) Answer(ctx context.Context, holds *core.Tickets, ref tickets.Ref, r Reply) (Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := j.Stored(ctx, ref)
	if err != nil {
		return Entry{}, err
	}
	open := openQuestions(entries)
	if len(open) == 0 {
		return Entry{}, ErrNoOpenQuestion
	}
	id := r.Question
	switch {
	case id != "":
		if !slices.Contains(open, id) {
			return Entry{}, &core.Error{Code: core.CodeInvalidState, Message: fmt.Sprintf("%s is not an open question of %s; open: %s", id, ref, strings.Join(open, ", "))}
		}
	default:
		id = open[0]
		for _, q := range open {
			if names(r.Text, q) {
				id = q
				break
			}
		}
	}
	e := Entry{Kind: Answer, Actor: r.Actor, ID: id, Text: r.Text, Link: r.Link}
	if err := j.append(ctx, ref, e); err != nil {
		return Entry{}, err
	}
	if len(open) > 1 {
		return e, nil
	}
	return e, liftNeedsInput(ctx, holds, r.Actor, ref.String())
}

// liftNeedsInput clears the ticket's hold when it is needs_input: the last
// open question has its answer (spec section 4.6). A hold of another reason
// stays.
func liftNeedsInput(ctx context.Context, holds *core.Tickets, actor, ref string) error {
	tk, err := holds.Get(ctx, ref)
	if err != nil || tk.Hold != needsInput {
		return err
	}
	_, err = holds.ClearHold(ctx, actor, ref, needsInput)
	var ce *core.Error
	if errors.As(err, &ce) && ce.Code == core.CodeInvalidState {
		return nil // lifted, or held for another reason, since it was read
	}
	return err
}

const needsInput = "needs_input"

// AnswerComment takes a trusted member's comment on the ticket as the answer
// to an open question (SyncOptions.Answer) and tells whether it did. A
// comment with a journal marker is istari's, or a copy of it: never an
// answer.
func (j *Journal) AnswerComment(ctx context.Context, holds *core.Tickets, ref tickets.Ref, c tickets.Comment) (bool, error) {
	if c.Own || strings.Contains(c.Body, markerStart) {
		return false, nil
	}
	_, err := j.Answer(ctx, holds, ref, Reply{Actor: "human:" + c.Author, Text: c.Body, Link: c.URL})
	if errors.Is(err, ErrNoOpenQuestion) {
		return false, nil
	}
	return err == nil, err
}

// openQuestions returns the ids of the questions without an answer, oldest
// first.
func openQuestions(entries []Entry) []string {
	answered := map[string]bool{}
	for _, e := range entries {
		if e.Kind == Answer {
			answered[e.ID] = true
		}
	}
	var open []string
	for _, e := range entries {
		if e.Kind == Question && !answered[e.ID] && !slices.Contains(open, e.ID) {
			open = append(open, e.ID)
		}
	}
	return open
}

// names tells whether text names the id as a word of its own: r_1-q1 is not
// named by r_1-q10.
func names(text, id string) bool {
	for i := 0; ; {
		k := strings.Index(text[i:], id)
		if k < 0 {
			return false
		}
		start, end := i+k, i+k+len(id)
		if (start == 0 || !idByte(text[start-1])) && (end == len(text) || !idByte(text[end])) {
			return true
		}
		i = start + 1
	}
}

func idByte(b byte) bool {
	return b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
