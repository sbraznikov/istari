package journal_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// asked is a ticket held needs_input with two open questions.
type asked struct {
	gh      *githubtest.Server
	n       int
	ref     tickets.Ref
	journal *journal.Journal
	tickets *core.Tickets
}

func askTwo(t *testing.T) asked {
	t.Helper()
	gh := githubtest.New(t)
	n := gh.Seed("Add a cache", "## Context\n\nWe need a cache.\n", "bob")
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ref := tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: n}
	tk := core.NewTickets(s, core.TicketOptions{Clock: func() time.Time { return at }})
	if _, err := tk.Create(ctx, "system", ref.String(), "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.SetHold(ctx, "agent:refiner-1", ref.String(), "needs_input"); err != nil {
		t.Fatal(err)
	}
	j := journal.New(github.New(github.Options{BaseURL: gh.URL, Token: gh.Token}), s, func() time.Time { return at })
	for _, q := range []journal.Entry{
		{Kind: journal.Question, Actor: "agent:refiner-1", ID: "r_1-q1", Text: "Is the type chore?"},
		{Kind: journal.Question, Actor: "agent:refiner-1", ID: "r_1-q2", Text: "Which size?"},
	} {
		if err := j.Append(ctx, ref, q); err != nil {
			t.Fatal(err)
		}
	}
	return asked{gh: gh, n: n, ref: ref, journal: j, tickets: tk}
}

func (a asked) answer(t *testing.T, r journal.Reply) error {
	t.Helper()
	_, err := a.journal.Answer(ctx, a.tickets, a.ref, r)
	return err
}

func (a asked) answers(t *testing.T) []journal.Entry {
	t.Helper()
	stored, err := a.journal.Stored(ctx, a.ref)
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Entry
	for _, e := range stored {
		if e.Kind == journal.Answer {
			e.At = time.Time{}
			out = append(out, e)
		}
	}
	return out
}

func (a asked) hold(t *testing.T) string {
	t.Helper()
	tk, err := a.tickets.Get(ctx, a.ref.String())
	if err != nil {
		t.Fatal(err)
	}
	return tk.Hold
}

func TestAnAnswerGoesToTheQuestionItsTextNamesElseToTheOldestOpenOne(t *testing.T) {
	t.Parallel()
	a := askTwo(t)
	if err := a.answer(t, journal.Reply{Actor: "human:alex", Text: "r_1-q2: size S.", Link: "https://github.com/x/1#c2"}); err != nil {
		t.Fatal(err)
	}
	if body := a.gh.Issue(a.n).Body; !strings.Contains(body, "Is the type chore?") || strings.Contains(body, "Which size?") {
		t.Fatalf("the Work state should list only the open question r_1-q1:\n%s", body)
	}
	if err := a.answer(t, journal.Reply{Actor: "human:bob", Text: "Yes, chore.", Link: "https://github.com/x/1#c3"}); err != nil {
		t.Fatal(err)
	}
	want := []journal.Entry{
		{Kind: journal.Answer, Actor: "human:alex", ID: "r_1-q2", Text: "r_1-q2: size S.", Link: "https://github.com/x/1#c2"},
		{Kind: journal.Answer, Actor: "human:bob", ID: "r_1-q1", Text: "Yes, chore.", Link: "https://github.com/x/1#c3"},
	}
	if got := a.answers(t); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("answers = %+v\nwant %+v", got, want)
	}
	if body := a.gh.Issue(a.n).Body; strings.Contains(body, "Is the type chore?") || strings.Contains(body, "Which size?") {
		t.Fatalf("the Work state still lists an answered question:\n%s", body)
	}
	if err := a.answer(t, journal.Reply{Actor: "human:alex", Text: "More."}); !errors.Is(err, journal.ErrNoOpenQuestion) {
		t.Fatalf("an answer without an open question: %v, want ErrNoOpenQuestion", err)
	}
}

func TestAnAnswerToAQuestionThatIsNotOpenIsRefused(t *testing.T) {
	t.Parallel()
	a := askTwo(t)
	if err := a.answer(t, journal.Reply{Question: "r_9-q1", Actor: "human:alex", Text: "No."}); err == nil {
		t.Fatal("an answer to an unknown question was recorded")
	}
	if err := a.answer(t, journal.Reply{Question: "r_1-q2", Actor: "human:alex", Text: "S."}); err != nil {
		t.Fatal(err)
	}
	if err := a.answer(t, journal.Reply{Question: "r_1-q2", Actor: "human:alex", Text: "M."}); err == nil {
		t.Fatal("a second answer to an answered question was recorded")
	}
	if got := a.answers(t); len(got) != 1 || got[0].ID != "r_1-q2" || got[0].Text != "S." {
		t.Fatalf("answers = %+v", got)
	}
}
