package runner_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

type pendingFixture struct {
	gh      *githubtest.Server
	db      string
	adapter *github.Provider
	now     time.Time
	logs    []string
	ref     tickets.Ref
}

func newPending(t *testing.T) *pendingFixture {
	t.Helper()
	gh := githubtest.New(t)
	n := gh.Seed("Rate limits", rawTicket, "bob")
	return &pendingFixture{gh: gh, db: filepath.Join(t.TempDir(), "istari.db"),
		adapter: github.New(github.Options{BaseURL: gh.URL, Token: gh.Token, Backoff: []time.Duration{}}),
		now:     time.Date(2026, 11, 25, 15, 45, 0, 0, time.UTC),
		ref:     tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: n}}
}

// open opens the store and the pending writes over it; the store closes with the test.
func (f *pendingFixture) open(t *testing.T) *runner.PendingWrites {
	t.Helper()
	s, err := store.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return runner.NewPendingWrites(s, runner.PendingOptions{Tracker: f.adapter, Clock: func() time.Time { return f.now },
		Logf: func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) }})
}

func TestAPendingCommentWaitsForTheTrackerAndIsDeliveredOnce(t *testing.T) {
	f := newPending(t)
	p := f.open(t)
	if err := p.Add(ctx, runner.PendingWrite{Kind: runner.PendingComment, Ticket: f.ref.String(), Body: "**Refined** — refiner-1: meets"}); err != nil {
		t.Fatal(err)
	}
	f.gh.FailNext(1)
	if err := p.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.gh.Issue(f.ref.Number).Comments; len(got) != 0 {
		t.Fatalf("comments while the tracker fails = %+v", got)
	}
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], "comment on "+f.ref.String()) || !strings.Contains(f.logs[0], "15:46:00") {
		t.Fatalf("log = %q, want one line naming the comment on %s and its next try at 15:46:00", f.logs, f.ref)
	}

	f.now = f.now.Add(time.Minute)
	for range 2 {
		if err := p.Replay(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := f.gh.Issue(f.ref.Number).Comments
	if len(got) != 1 || !strings.HasPrefix(got[0].Body, "**Refined** — refiner-1: meets") {
		t.Fatalf("comments after the tracker is back = %+v, want the refined comment once", got)
	}
	if left, err := p.List(ctx); err != nil || len(left) != 0 {
		t.Fatalf("pending after delivery = %+v, %v", left, err)
	}
}

func TestAPendingWriteOutlivesARestartOfTheDaemon(t *testing.T) {
	f := newPending(t)
	first := f.open(t)
	for _, w := range []runner.PendingWrite{
		{Kind: runner.PendingSize, Ticket: f.ref.String(), Size: "M"},
		{Kind: runner.PendingComment, Ticket: f.ref.String(), Body: "second"},
	} {
		if err := first.Add(ctx, w); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(time.Second)
	}
	got, err := f.open(t).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != runner.PendingSize || got[1].Body != "second" {
		t.Fatalf("after reopening: %+v, want the size, then the comment", got)
	}
}
