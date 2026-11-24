// Package api serves the daemon API over HTTP + JSON and the event stream (spec section 13).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/scheduler"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
)

// Server serves the daemon API over HTTP + JSON:
//
//	GET  /v1/health
//	POST /v1/<operation>        one core operation; the body is its JSON request
//	GET  /v1/events             a page of events: after, project, ticket, type, limit
//	GET  /v1/events/stream      the same as server-sent events, live
//
// Operations are RPC-style rather than resource paths because ticket
// references contain '/' and '#'. A refusal by the core answers with its code:
// 404 for not_found, 409 for the others; a malformed request answers 400.
type Server struct {
	store   store.Store
	tasks   *core.Tasks
	tickets *core.Tickets
	sched   *scheduler.Scheduler
	syncer  *tickets.Syncer
	ops     map[string]operation
	changed *notifier
	clock   func() time.Time
	keys    keyLocks
	closing chan struct{}
	once    sync.Once
	mux     *http.ServeMux
}

// ServerOptions configures a Server.
type ServerOptions struct {
	Clock  func() time.Time
	Limits core.Limits
	Tick   time.Duration // scheduler tick; 0 means scheduler.DefaultTick
	// Tracker and Repos turn on the ticket sync; without a tracker there is none.
	Tracker tickets.SyncProvider
	Repos   []tickets.SyncRepo
	// Standards turns on admission and the re-check on edit.
	Standards tickets.Standards
}

// NewServer returns the API on top of a store. Every append made through the
// server wakes the event streams.
func NewServer(s store.Store, opts ServerOptions) *Server {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	n := newNotifier()
	ns := notifyingStore{Store: s, n: n}
	srv := &Server{
		store:   ns,
		tasks:   core.NewTasks(ns, core.TaskOptions{Clock: opts.Clock, Limits: opts.Limits}),
		tickets: core.NewTickets(ns, core.TicketOptions{Clock: opts.Clock}),
		changed: n,
		clock:   opts.Clock,
		closing: make(chan struct{}),
		mux:     http.NewServeMux(),
	}
	srv.sched = scheduler.New(ns, srv.tasks, srv.Do, scheduler.Options{Clock: opts.Clock, Tick: opts.Tick, Prepare: srv.Prepare})
	if opts.Tracker != nil {
		srv.syncer = tickets.NewSyncer(opts.Tracker, srv.tickets, ns, opts.Repos, tickets.SyncOptions{Clock: opts.Clock, Standards: opts.Standards})
	}
	srv.ops = srv.operations()
	srv.mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	srv.mux.HandleFunc("GET /v1/events", srv.events)
	srv.mux.HandleFunc("GET /v1/events/stream", srv.stream)
	for name, op := range srv.ops {
		srv.mux.HandleFunc("POST /v1/"+name, srv.handle(name, op))
	}
	return srv
}

// Store returns the store as the server writes it: appends wake the streams.
func (s *Server) Store() store.Store { return s.store }

// Tasks returns the task machine.
func (s *Server) Tasks() *core.Tasks { return s.tasks }

// Tickets returns the ticket machine.
func (s *Server) Tickets() *core.Tickets { return s.tickets }

// Syncer returns the ticket sync, or nil when no tracker is configured.
func (s *Server) Syncer() *tickets.Syncer { return s.syncer }

// Scheduler returns the scheduler; the daemon runs it.
func (s *Server) Scheduler() *scheduler.Scheduler { return s.sched }

// Prepare checks a write request for later: op must be a write operation and
// request must decode and validate as its request. A request without an actor
// gets actor when the operation's request has one.
func (s *Server) Prepare(op string, request json.RawMessage, actor string) (json.RawMessage, error) {
	o, ok := s.ops[op]
	if !ok || o.again == nil {
		return nil, fmt.Errorf("%q is not a write operation", op)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(request, &fields); err != nil {
		return nil, fmt.Errorf("the request is not a JSON object: %w", err)
	}
	if _, named := fields["actor"]; !named {
		fields["actor"], _ = json.Marshal(actor)
		withActor, _ := json.Marshal(fields)
		if o.check(withActor) == nil {
			return withActor, nil
		}
	}
	if err := o.check(request); err != nil {
		return nil, err
	}
	return request, nil
}

// Do performs a write operation as an HTTP request with an idempotency key
// would. Refusals and malformed requests come back as *core.Error.
func (s *Server) Do(ctx context.Context, op, key string, request json.RawMessage) error {
	o, ok := s.ops[op]
	if !ok || o.again == nil {
		return &core.Error{Code: "bad_request", Message: fmt.Sprintf("%q is not a write operation", op)}
	}
	var err error
	if key != "" {
		_, err = s.idempotent(ctx, key, op, o, request)
	} else {
		_, err = o.run(ctx, request)
	}
	var bad errBadRequest
	if errors.As(err, &bad) {
		return &core.Error{Code: "bad_request", Message: bad.Error(), Err: err}
	}
	return err
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Close ends the event streams; ordinary requests are not affected.
func (s *Server) Close() { s.once.Do(func() { close(s.closing) }) }

// operation is one API operation. run decodes its request from raw and returns
// the response; check only decodes and validates. A write has again, which
// answers a repeat of an idempotency key from the current projections; reads
// have none.
type operation struct {
	run   func(ctx context.Context, raw json.RawMessage) (any, error)
	check func(raw json.RawMessage) error
	again func(ctx context.Context, done requestRecord) (any, error)
}

func read[Req any](f func(ctx context.Context, req Req) (any, error)) operation {
	return operation{run: decode(f), check: checker[Req]()}
}

func write[Req any](again func(context.Context, requestRecord) (any, error), f func(ctx context.Context, req Req) (any, error)) operation {
	return operation{run: decode(f), check: checker[Req](), again: again}
}

func checker[Req any]() func(json.RawMessage) error {
	run := decode(func(context.Context, Req) (any, error) { return nil, nil })
	return func(raw json.RawMessage) error {
		_, err := run(context.Background(), raw)
		return err
	}
}

// errBadRequest marks a request the server could not understand.
type errBadRequest struct{ err error }

func (e errBadRequest) Error() string { return e.err.Error() }

// validator is a request that checks itself, e.g. that it names an actor.
type validator interface{ validate() error }

// decode adapts a typed operation: it decodes the request strictly (unknown
// fields are an error) and validates it.
func decode[Req any](f func(ctx context.Context, req Req) (any, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req Req
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return nil, errBadRequest{fmt.Errorf("decoding request: %w", err)}
		}
		if v, ok := any(&req).(validator); ok {
			if err := v.validate(); err != nil {
				return nil, errBadRequest{err}
			}
		}
		return f(ctx, req)
	}
}

// maxRequestBytes bounds a request body.
const maxRequestBytes = 1 << 20

func (s *Server) handle(name string, op operation) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			writeError(w, errBadRequest{fmt.Errorf("reading request: %w", err)})
			return
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			raw = []byte("{}") // no body is an empty request
		}
		var resp any
		key := r.Header.Get(IdempotencyHeader)
		if strings.HasPrefix(key, timerKeyPrefix) {
			writeError(w, errBadRequest{fmt.Errorf("idempotency keys starting with %q belong to the scheduler", timerKeyPrefix)})
			return
		}
		if key != "" && op.again != nil {
			resp, err = s.idempotent(r.Context(), key, name, op, raw)
		} else {
			resp, err = op.run(r.Context(), raw)
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// ErrorBody is the JSON body of every failed request.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, err error) {
	var coreErr *core.Error
	var bad errBadRequest
	switch {
	case errors.Is(err, store.ErrInvalidCursor):
		writeJSON(w, http.StatusBadRequest, ErrorBody{Code: "bad_request", Message: err.Error()})
	case errors.As(err, &coreErr):
		status := http.StatusConflict
		if coreErr.Code == core.CodeNotFound {
			status = http.StatusNotFound
		}
		writeJSON(w, status, ErrorBody{Code: coreErr.Code, Message: coreErr.Message})
	case errors.As(err, &bad):
		writeJSON(w, http.StatusBadRequest, ErrorBody{Code: "bad_request", Message: bad.Error()})
	default:
		log.Printf("istari: internal error: %v", err)
		writeJSON(w, http.StatusInternalServerError, ErrorBody{Code: "internal", Message: err.Error()})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
