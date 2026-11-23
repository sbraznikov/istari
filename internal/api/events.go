package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/store"
)

// pageSize bounds one read of the log.
const pageSize = 500

// pollEvery wakes a stream even without a notification, and keeps the
// connection alive with a comment line.
const pollEvery = time.Second

// writeTimeout bounds one write to a stream, so a stalled client cannot hold
// the daemon's shutdown.
const writeTimeout = 10 * time.Second

func filterOf(r *http.Request) (store.Cursor, store.Filter) {
	q := r.URL.Query()
	return store.Cursor(q.Get("after")), store.Filter{
		Project: q.Get("project"), Ticket: q.Get("ticket"), TypePrefix: q.Get("type"),
	}
}

// events answers one page of the log as a JSON array.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after, filter := filterOf(r)
	limit := pageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, errBadRequest{fmt.Errorf("limit %q is not a positive number", v)})
			return
		}
		limit = min(n, pageSize)
	}
	events, err := s.store.Read(r.Context(), after, filter, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if events == nil {
		events = []store.Event{}
	}
	writeJSON(w, http.StatusOK, events)
}

// stream sends every event after the cursor as server-sent events, then waits
// for new ones. The SSE id is the event cursor, so a client that reconnects
// with Last-Event-ID (or ?after=) continues exactly after the last event it
// saw: delivery is at least once and in log order.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	after, filter := filterOf(r)
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		after = store.Cursor(id)
	}
	// Check the cursor before the headers, so a bad one is a 400 and not a
	// stream that ends at once and is retried forever.
	if _, err := s.store.Read(r.Context(), after, filter, 1); err != nil {
		writeError(w, err)
		return
	}
	rc := http.NewResponseController(w)
	flush := func() error {
		if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
		return rc.Flush()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if flush() != nil {
		return
	}
	for {
		wake := s.changed.wait() // before reading, so no append is missed
		events, err := s.store.Read(r.Context(), after, filter, pageSize)
		if err != nil {
			return // the client sees the stream end and reconnects
		}
		for _, e := range events {
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.Cursor, e.Type, data)
			after = e.Cursor
		}
		if len(events) > 0 && flush() != nil {
			return
		}
		if len(events) == pageSize {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-wake:
		case <-time.After(pollEvery):
			fmt.Fprint(w, ": ping\n\n")
			if flush() != nil {
				return
			}
		}
	}
}

// notifier wakes every waiter when the log changes.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: make(chan struct{})} }

// wait returns a channel that is closed at the next notify.
func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}

// notifyingStore notifies after every successful append.
type notifyingStore struct {
	store.Store
	n *notifier
}

func (s notifyingStore) Append(ctx context.Context, e store.Event, changes ...store.Change) (store.Event, error) {
	e, err := s.Store.Append(ctx, e, changes...)
	if err == nil {
		s.n.notify()
	}
	return e, err
}
