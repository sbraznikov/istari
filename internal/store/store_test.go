package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sbraznikov/istari/internal/store"
)

var ctx = context.Background()

func open(t *testing.T, path string) *store.SQLite {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "istari.db")
}

func mustAppend(t *testing.T, s store.Store, e store.Event, changes ...store.Change) store.Event {
	t.Helper()
	got, err := s.Append(ctx, e, changes...)
	if err != nil {
		t.Fatalf("Append(%s): %v", e.Type, err)
	}
	return got
}

func readAll(t *testing.T, s store.Store, after store.Cursor, f store.Filter) []store.Event {
	t.Helper()
	events, err := s.Read(ctx, after, f, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return events
}

func types(events []store.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func TestAppendedEventIsReadBackWithCursorAndTime(t *testing.T) {
	s := open(t, tempDB(t))
	appended := mustAppend(t, s, store.Event{
		Type:    "task.claimed",
		Actor:   "agent:implementer-2",
		Project: "web",
		Ticket:  "gh:acme/web#123",
		Task:    "t_91",
		Run:     "r_207",
		Payload: json.RawMessage(`{"lease_until":"2026-11-24T10:05:00Z"}`),
	})
	if appended.Cursor == "" {
		t.Fatal("appended event has no cursor")
	}
	if appended.At.IsZero() {
		t.Fatal("appended event has no time")
	}

	events := readAll(t, s, "", store.Filter{})
	if len(events) != 1 {
		t.Fatalf("Read returned %d events, want 1", len(events))
	}
	got := events[0]
	if got.Cursor != appended.Cursor || !got.At.Equal(appended.At) {
		t.Errorf("read cursor/at = %s/%s, want %s/%s", got.Cursor, got.At, appended.Cursor, appended.At)
	}
	if got.Type != "task.claimed" || got.Actor != "agent:implementer-2" || got.Project != "web" ||
		got.Ticket != "gh:acme/web#123" || got.Task != "t_91" || got.Run != "r_207" {
		t.Errorf("read event = %+v, fields differ from the appended one", got)
	}
	if string(got.Payload) != `{"lease_until":"2026-11-24T10:05:00Z"}` {
		t.Errorf("payload = %s", got.Payload)
	}
}

func TestReadReturnsExactlyTheEventsAfterTheCursorInOrder(t *testing.T) {
	s := open(t, tempDB(t))
	var cursors []store.Cursor
	for _, typ := range []string{"a.1", "a.2", "a.3", "a.4"} {
		cursors = append(cursors, mustAppend(t, s, store.Event{Type: typ}).Cursor)
	}
	if got := types(readAll(t, s, cursors[1], store.Filter{})); !equal(got, []string{"a.3", "a.4"}) {
		t.Errorf("after the 2nd event: %v, want [a.3 a.4]", got)
	}
	if got := readAll(t, s, cursors[3], store.Filter{}); len(got) != 0 {
		t.Errorf("after the last event: %v, want none", types(got))
	}
	page, err := s.Read(ctx, "", store.Filter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := types(page); !equal(got, []string{"a.1", "a.2"}) {
		t.Errorf("first page of 2: %v, want [a.1 a.2]", got)
	}
}

func TestFiltersReturnOnlyMatchingEvents(t *testing.T) {
	s := open(t, tempDB(t))
	for _, e := range []store.Event{
		{Type: "task.created", Project: "web", Ticket: "gh:acme/web#1"},
		{Type: "ticket.transitioned", Project: "web", Ticket: "gh:acme/web#1"},
		{Type: "task.claimed", Project: "web", Ticket: "gh:acme/web#2"},
		{Type: "task.created", Project: "billing", Ticket: "gh:acme/billing-core#7"},
		{Type: "tasks.odd", Project: "web"},
	} {
		mustAppend(t, s, e)
	}
	for _, tc := range []struct {
		name   string
		filter store.Filter
		want   []string
	}{
		{"project", store.Filter{Project: "billing"}, []string{"task.created"}},
		{"ticket", store.Filter{Ticket: "gh:acme/web#1"}, []string{"task.created", "ticket.transitioned"}},
		{"type prefix", store.Filter{TypePrefix: "task."}, []string{"task.created", "task.claimed", "task.created"}},
		{"combined", store.Filter{Project: "web", TypePrefix: "task."}, []string{"task.created", "task.claimed"}},
	} {
		if got := types(readAll(t, s, "", tc.filter)); !equal(got, tc.want) {
			t.Errorf("%s filter: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChangesAreAppliedWithTheirEvent(t *testing.T) {
	s := open(t, tempDB(t))
	mustAppend(t, s, store.Event{Type: "task.created", Task: "t_1"},
		store.Change{Kind: "task", ID: "t_1", Data: json.RawMessage(`{"state":"ready"}`)})
	mustAppend(t, s, store.Event{Type: "task.claimed", Task: "t_1"},
		store.Change{Kind: "task", ID: "t_1", Data: json.RawMessage(`{"state":"claimed"}`), IfVersion: 1})

	rec, err := s.Get(ctx, "task", "t_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Version != 2 || string(rec.Data) != `{"state":"claimed"}` {
		t.Errorf("record = version %d %s, want version 2 {\"state\":\"claimed\"}", rec.Version, rec.Data)
	}
	if _, err := s.Get(ctx, "task", "t_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get of a missing record: %v, want ErrNotFound", err)
	}
}

func TestAFailedChangeRollsBackItsEventAndTheOtherChanges(t *testing.T) {
	s := open(t, tempDB(t))
	mustAppend(t, s, store.Event{Type: "task.created", Task: "t_1"},
		store.Change{Kind: "task", ID: "t_1", Data: json.RawMessage(`{"state":"ready"}`)})

	// The second change is stale: t_1 is at version 1, not 7.
	_, err := s.Append(ctx, store.Event{Type: "task.claimed", Task: "t_1"},
		store.Change{Kind: "task", ID: "t_2", Data: json.RawMessage(`{"state":"ready"}`)},
		store.Change{Kind: "task", ID: "t_1", Data: json.RawMessage(`{"state":"claimed"}`), IfVersion: 7})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Append with a stale change: %v, want ErrConflict", err)
	}
	if got := types(readAll(t, s, "", store.Filter{})); !equal(got, []string{"task.created"}) {
		t.Errorf("log = %v, want only [task.created]", got)
	}
	if rec, _ := s.Get(ctx, "task", "t_1"); rec.Version != 1 || string(rec.Data) != `{"state":"ready"}` {
		t.Errorf("t_1 = version %d %s, want it unchanged", rec.Version, rec.Data)
	}
	if _, err := s.Get(ctx, "task", "t_2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("t_2 was written although its event rolled back: %v", err)
	}
	// Creating a record that already exists conflicts too.
	_, err = s.Append(ctx, store.Event{Type: "task.created", Task: "t_1"},
		store.Change{Kind: "task", ID: "t_1", Data: json.RawMessage(`{}`)})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("creating an existing record: %v, want ErrConflict", err)
	}
}

func TestListReturnsTheRecordsOfOneKindByID(t *testing.T) {
	s := open(t, tempDB(t))
	for _, c := range []store.Change{
		{Kind: "task", ID: "t_2", Data: json.RawMessage(`2`)},
		{Kind: "ticket", ID: "gh:acme/web#1", Data: json.RawMessage(`0`)},
		{Kind: "task", ID: "t_1", Data: json.RawMessage(`1`)},
	} {
		mustAppend(t, s, store.Event{Type: c.Kind + ".created"}, c)
	}
	recs, err := s.List(ctx, "task")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.ID+"="+string(r.Data))
	}
	if !equal(ids, []string{"t_1=1", "t_2=2"}) {
		t.Errorf("List(task) = %v, want [t_1=1 t_2=2]", ids)
	}
}

func TestConcurrentAppendsGetIncreasingCursorsAndNoneIsLost(t *testing.T) {
	path := tempDB(t)
	handles := []*store.SQLite{open(t, path), open(t, path)} // two handles stand in for two processes
	const writers, perWriter = 8, 25
	appended := make(chan store.Cursor, writers*perWriter)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				e, err := handles[w%2].Append(ctx, store.Event{Type: fmt.Sprintf("w%d.%d", w, i)})
				if err != nil {
					t.Errorf("Append: %v", err)
					return
				}
				appended <- e.Cursor
			}
		}()
	}
	wg.Wait()
	close(appended)

	log := readAll(t, handles[0], "", store.Filter{})
	if len(log) != writers*perWriter {
		t.Fatalf("log has %d events, want %d", len(log), writers*perWriter)
	}
	inLog := map[store.Cursor]int{}
	for i, e := range log {
		if _, dup := inLog[e.Cursor]; dup {
			t.Fatalf("cursor %s appears twice", e.Cursor)
		}
		inLog[e.Cursor] = i
	}
	for c := range appended {
		i, ok := inLog[c]
		if !ok {
			t.Fatalf("appended cursor %s is not in the log", c)
		}
		if after := readAll(t, handles[1], c, store.Filter{}); len(after) != len(log)-1-i {
			t.Fatalf("Read(after %s) = %d events, want %d", c, len(after), len(log)-1-i)
		}
	}
	// Within one writer, later appends come later in the log.
	last := map[string]int{}
	for i, e := range log {
		var w, n int
		fmt.Sscanf(e.Type, "w%d.%d", &w, &n)
		key := fmt.Sprint(w)
		if prev, ok := last[key]; ok && prev > i {
			t.Fatalf("writer %d's events are out of order", w)
		}
		last[key] = i
	}
}

func TestReopeningPreservesTheLogAndTheProjections(t *testing.T) {
	path := tempDB(t)
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := mustAppend(t, s, store.Event{Type: "task.created", Task: "t_1"},
		store.Change{Kind: "task", ID: "t_1", Data: json.RawMessage(`{"state":"ready"}`)})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, path)
	if got := readAll(t, s, "", store.Filter{}); len(got) != 1 || got[0].Cursor != first.Cursor {
		t.Fatalf("after reopen the log is %v, want the one event", types(got))
	}
	rec, err := s.Get(ctx, "task", "t_1")
	if err != nil || rec.Version != 1 {
		t.Fatalf("after reopen t_1 = %+v, %v", rec, err)
	}
	second := mustAppend(t, s, store.Event{Type: "task.claimed", Task: "t_1"})
	if after := readAll(t, s, first.Cursor, store.Filter{}); len(after) != 1 || after[0].Cursor != second.Cursor {
		t.Fatalf("an event appended after reopen does not follow the old ones")
	}
}
