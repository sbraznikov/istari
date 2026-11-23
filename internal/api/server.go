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
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/scheduler"
	"github.com/sbraznikov/istari/internal/store"
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
	ops     map[string]operation
	changed *notifier
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
		closing: make(chan struct{}),
		mux:     http.NewServeMux(),
	}
	srv.sched = scheduler.New(ns, srv.tasks, srv.Do, scheduler.Options{Clock: opts.Clock, Tick: opts.Tick, Allowed: srv.IsWrite})
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

// Scheduler returns the scheduler; the daemon runs it.
func (s *Server) Scheduler() *scheduler.Scheduler { return s.sched }

// IsWrite tells whether op is a write operation.
func (s *Server) IsWrite(op string) bool {
	o, ok := s.ops[op]
	return ok && o.again != nil
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
// the response. A write has again, which answers a repeat of an idempotency
// key from the current projections; reads have none.
type operation struct {
	run   func(ctx context.Context, raw json.RawMessage) (any, error)
	again func(ctx context.Context, done requestRecord) (any, error)
}

func read[Req any](f func(ctx context.Context, req Req) (any, error)) operation {
	return operation{run: decode(f)}
}

func write[Req any](again func(context.Context, requestRecord) (any, error), f func(ctx context.Context, req Req) (any, error)) operation {
	return operation{run: decode(f), again: again}
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
		if key := r.Header.Get(IdempotencyHeader); key != "" && op.again != nil {
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
