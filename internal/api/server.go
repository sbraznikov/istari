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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sbraznikov/istari/internal/config"
	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/scheduler"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/tickets/standard"
	"github.com/sbraznikov/istari/internal/vcs"
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
	store     store.Store
	tasks     *core.Tasks
	tickets   *core.Tickets
	sched     *scheduler.Scheduler
	syncer    *tickets.Syncer
	spaces    *runner.Workspaces
	runs      *runner.Runs
	runner    *runner.Runner
	engine    *pipeline.Engine
	vcs       *vcs.Client
	watch     *vcs.Watcher
	saves     *runner.Checkpoints
	refines   *runner.Refines
	plans     *runner.Plans
	reviews   *runner.Reviews
	prs       *runner.PullRequests
	tracker   tickets.SyncProvider
	firstRepo string          // the first configured repository: where a plan goes without its own
	repos     map[string]bool // the configured repositories: only their tickets get worktrees
	ops       map[string]operation
	changed   *notifier
	clock     func() time.Time
	keys      keyLocks
	closing   chan struct{}
	once      sync.Once
	mux       *http.ServeMux
}

// ServerOptions configures a Server.
type ServerOptions struct {
	Clock  func() time.Time
	Limits core.Limits
	// Priority is the default ticket priority; "" means core.DefaultPriority.
	Priority string
	Tick     time.Duration // scheduler tick; 0 means scheduler.DefaultTick
	// Tracker and Repos turn on the ticket sync; without a tracker there is none.
	Tracker tickets.SyncProvider
	Repos   []tickets.SyncRepo
	// Standards turns on admission and the re-check on edit.
	Standards tickets.Standards
	// Workspaces turns on ticket worktrees when its DataDir is set.
	Workspaces runner.WorkspaceOptions
	// Runner turns on the runner when it has slots; it needs workspaces.
	Runner runner.RunnerOptions
	// Defaults are the installation's pipeline defaults; the repositories'
	// pipeline files are read from the tracker when it has them.
	Defaults config.Defaults
	// VCS is the GitHub side: pushes, pull requests, checks and reviews.
	VCS *vcs.Client
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
		tasks:   core.NewTasks(ns, core.TaskOptions{Clock: opts.Clock, Limits: opts.Limits, DefaultPriority: opts.Priority}),
		tickets: core.NewTickets(ns, core.TicketOptions{Clock: opts.Clock}),
		changed: n,
		clock:   opts.Clock,
		closing: make(chan struct{}),
		mux:     http.NewServeMux(),
	}
	srv.sched = scheduler.New(ns, srv.tasks, srv.Do, scheduler.Options{Clock: opts.Clock, Tick: opts.Tick, Prepare: srv.Prepare})
	if opts.Tracker != nil {
		var jt journal.Tracker
		if t, ok := opts.Tracker.(journal.Tracker); ok {
			jt = t
		}
		j := journal.New(jt, ns, opts.Clock)
		note := func(ctx context.Context, ref tickets.Ref, text string) error {
			return j.Append(ctx, ref, journal.Entry{Kind: journal.Note, Actor: "system", Text: text})
		}
		srv.syncer = tickets.NewSyncer(opts.Tracker, srv.tickets, ns, opts.Repos, tickets.SyncOptions{Clock: opts.Clock, Standards: opts.Standards, Needs: standard.Needs, Parent: standard.Parent, Note: note, Merged: srv.merged})
	}
	srv.tracker = opts.Tracker
	if len(opts.Repos) > 0 {
		srv.firstRepo = opts.Repos[0].Repo
	}
	srv.repos = map[string]bool{}
	for _, r := range opts.Repos {
		srv.repos[strings.ToLower(r.Repo)] = true
	}
	if opts.Workspaces.DataDir != "" {
		ws := opts.Workspaces
		ws.Clock = opts.Clock
		srv.spaces = runner.NewWorkspaces(ns, ws)
		// The journal writes to the tracker when it can, else to istari's log.
		var jt journal.Tracker
		if t, ok := opts.Tracker.(journal.Tracker); ok {
			jt = t
		}
		var tokens vcs.Tokens
		if opts.VCS != nil {
			tokens = opts.VCS.Tokens()
		}
		srv.saves = runner.NewCheckpoints(srv.spaces, journal.New(jt, ns, opts.Clock), tokens)
		if len(opts.Runner.Slots) > 0 {
			ro := opts.Runner
			ro.Clock = opts.Clock
			ro.Checkpoints = srv.saves
			srv.runner = runner.New(ns, srv.tasks, srv.tickets, srv.spaces, ro)
		}
	}
	srv.runs = runner.NewRuns(ns, srv.tasks, opts.Clock)
	var files pipeline.Files
	if f, ok := opts.Tracker.(pipeline.Files); ok {
		files = f
	}
	if opts.VCS != nil {
		srv.vcs = opts.VCS
		srv.watch = vcs.NewWatcher(opts.VCS, ns, opts.Clock)
	}
	var needsRefine func(ctx context.Context, tk core.Ticket) (bool, error)
	if opts.Tracker != nil && opts.Standards != nil {
		needsRefine = func(ctx context.Context, tk core.Ticket) (bool, error) {
			ref, err := tickets.ParseRef(tk.Ref)
			if err != nil {
				return false, nil
			}
			t, err := opts.Tracker.Get(ctx, ref)
			if err != nil {
				return false, err
			}
			conf, err := opts.Standards.Check(ctx, ref.Repo, t)
			if err != nil || len(conf.Deviations) == 0 {
				return false, err
			}
			// Only a ticket that would be admitted costs a refiner run (spec 4.5).
			return srv.syncer.Refinable(ctx, tk.Ref)
		}
		if rt, ok := opts.Tracker.(runner.RefineTracker); ok {
			var jt journal.Tracker
			if t, ok := opts.Tracker.(journal.Tracker); ok {
				jt = t
			}
			srv.refines = runner.NewRefines(rt, journal.New(jt, ns, opts.Clock), srv.tickets, opts.Standards)
		}
	}
	hasPlan := func(ctx context.Context, ref string) (bool, error) {
		r, err := tickets.ParseRef(ref)
		if err != nil {
			return false, nil
		}
		entries, err := journal.New(nil, ns, nil).Stored(ctx, r)
		return slices.ContainsFunc(entries, func(e journal.Entry) bool { return e.Kind == journal.Plan }), err
	}
	var merged func(ctx context.Context, ref string) (bool, error)
	var checks func(ctx context.Context, ref string) (string, bool)
	var ready func(ctx context.Context, ref string) error
	if srv.watch != nil {
		merged = srv.watch.SeenMerged
		checks = func(ctx context.Context, ref string) (string, bool) {
			pr, err := srv.watch.PullRequestOf(ctx, ref)
			if err != nil || pr.Checks == "" {
				return "", false
			}
			return pr.Checks, true
		}
		// The draft ends on green CI, before T6 (spec 4.3, stage verify).
		ready = func(ctx context.Context, ref string) error {
			pr, err := srv.watch.PullRequestOf(ctx, ref)
			if err != nil {
				return err
			}
			return opts.VCS.MarkReady(ctx, pr.Repo, pr.Number)
		}
	}
	srv.engine = pipeline.NewEngine(srv.tasks, srv.tickets, pipeline.EngineOptions{Defaults: opts.Defaults, Files: files, Clock: opts.Clock, NeedsRefine: needsRefine, HasPlan: hasPlan, Checks: checks, Ready: ready, Review: srv.reviewOf, Merged: merged})
	if srv.runner != nil {
		srv.runner.SetSelfReview(func(ctx context.Context, repo string) bool {
			s, err := srv.engine.SettingsOf(ctx, repo)
			return err != nil || s.SelfReview
		})
	}
	if srv.saves != nil && srv.vcs != nil {
		srv.prs = runner.NewPullRequests(srv.saves, srv.vcs, srv.watch)
	}
	if srv.spaces != nil {
		var jt journal.Tracker
		if t, ok := opts.Tracker.(journal.Tracker); ok {
			jt = t
		}
		var sizer runner.Sizer
		if t, ok := opts.Tracker.(runner.RefineTracker); ok {
			sizer = t
		}
		srv.plans = runner.NewPlans(srv.spaces, journal.New(jt, ns, opts.Clock), srv.tickets, sizer)
		srv.reviews = runner.NewReviews(srv.spaces, journal.New(jt, ns, opts.Clock))
		if srv.syncer != nil {
			srv.plans.WithSplit(srv.tasks, opts.Tracker, srv.syncer.Created)
		}
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

// RunWorkspaces moves every ticket of a configured repository through its
// pipeline and gives it the worktree its state calls for, at once
// and after every change, and once a minute to repair what was missed, until
// ctx ends. Without workspaces it returns at once.
func (s *Server) RunWorkspaces(ctx context.Context) {
	if len(s.repos) == 0 {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		changed := s.changed.wait()
		// The pipeline first: a ticket it starts gets its worktree at once.
		all, err := s.tickets.List(ctx)
		if err == nil {
			err = s.engine.Reconcile(ctx, s.configured(all))
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("istari: pipeline: %v", err)
		}
		if s.spaces != nil {
			all, err := s.tickets.List(ctx)
			if err == nil {
				err = s.spaces.Reconcile(ctx, s.configured(all))
			}
			if err != nil && ctx.Err() == nil {
				log.Printf("istari: workspaces: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-ticker.C:
		}
	}
}

// configured returns the tickets of the configured repositories.
func (s *Server) configured(all []core.Ticket) []core.Ticket {
	var out []core.Ticket
	for _, tk := range all {
		if ref, err := tickets.ParseRef(tk.Ref); err == nil && s.repos[strings.ToLower(ref.Repo)] {
			out = append(out, tk)
		}
	}
	return out
}

// merged tells sync whether a ticket's pull request is merged (spec 4.8):
// fresh asks GitHub while the PR watch has not seen it closed yet.
func (s *Server) merged(ctx context.Context, ref string, fresh bool) (bool, error) {
	switch {
	case s.watch == nil:
		return false, nil
	case fresh:
		return s.watch.Merged(ctx, ref)
	default:
		return s.watch.SeenMerged(ctx, ref)
	}
}

// RunVCS polls the tracked pull requests every interval until ctx ends: their
// checks and reviews become events. Without VCS it returns at once.
func (s *Server) RunVCS(ctx context.Context, every time.Duration) {
	if s.watch == nil {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := s.watch.Poll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("istari: pull requests: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Runner returns the runner, or nil when it is off; the daemon runs it.
func (s *Server) Runner() *runner.Runner { return s.runner }

// ReasonDaemonRestarted is how a run ends that a daemon left running.
const ReasonDaemonRestarted = "the daemon restarted"

// EndRunsLeftRunning ends the runs a previous daemon left running; the
// daemon calls it on start, before the runner starts a run (spec section 7.2).
func (s *Server) EndRunsLeftRunning(ctx context.Context) error {
	return s.runs.EndLeftRunning(ctx, ReasonDaemonRestarted)
}

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
		ctx := r.Context()
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			// An agent's request: it acts as its run's agent, on its run's
			// ticket only (spec sections 7.2 and 11).
			var run runner.Run
			if run, raw, err = s.asAgent(ctx, name, raw, token); err != nil {
				writeError(w, err)
				return
			}
			ctx = context.WithValue(ctx, runKey{}, run)
		}
		var resp any
		key := r.Header.Get(IdempotencyHeader)
		if strings.HasPrefix(key, timerKeyPrefix) {
			writeError(w, errBadRequest{fmt.Errorf("idempotency keys starting with %q belong to the scheduler", timerKeyPrefix)})
			return
		}
		if key != "" && op.again != nil {
			resp, err = s.idempotent(ctx, key, name, op, raw)
		} else {
			resp, err = op.run(ctx, raw)
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
		switch coreErr.Code {
		case core.CodeNotFound:
			status = http.StatusNotFound
		case runner.CodeTokenInvalid:
			status = http.StatusUnauthorized
		case runner.CodeTokenScope, CodeTokenForbidden, vcs.CodePushRefused:
			status = http.StatusForbidden
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
