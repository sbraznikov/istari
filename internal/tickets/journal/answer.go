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
// questions and returns the entry.
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
	return e, nil
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
