package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sbraznikov/istari/internal/core"
)

// The loops a stepped daemon runs one pass of per POST /v1/step (spec 3.1).
const (
	LoopScheduler = "scheduler" // lease expiry and due timers
	LoopPipeline  = "pipeline"  // the pipeline engine, then the worktrees
	LoopSync      = "sync"      // the ticket sync
	LoopWatch     = "watch"     // the PR watch
)

// passes maps each loop name to its one pass: the one place that names the
// loops, for both the check of a request and the step itself.
var passes = map[string]func(*Server, context.Context) error{
	LoopScheduler: (*Server).stepScheduler,
	LoopPipeline:  (*Server).stepPipeline,
	LoopSync:      (*Server).stepSync,
	LoopWatch:     (*Server).stepWatch,
}

type stepRequest struct {
	Loop string `json:"loop"`
}

func (r stepRequest) validate() error {
	if _, ok := passes[r.Loop]; ok {
		return nil
	}
	return fmt.Errorf("loop %q is not %s, %s, %s or %s", r.Loop, LoopScheduler, LoopPipeline, LoopSync, LoopWatch)
}

// Step runs one pass of loop and returns when it has finished. Steps run one
// at a time, as each timed loop runs one pass at a time. A loop the daemon
// has not configured, such as sync without a tracker, is refused.
func (s *Server) Step(ctx context.Context, loop string) error {
	req := stepRequest{Loop: loop}
	if err := req.validate(); err != nil {
		return errBadRequest{err}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return passes[loop](s, ctx)
}

func (s *Server) stepScheduler(ctx context.Context) error { return s.sched.Step(ctx) }

func (s *Server) stepPipeline(ctx context.Context) error {
	if len(s.repos) == 0 {
		return &core.Error{Code: core.CodeInvalidState, Message: "no pipeline to step: no project has a repository"}
	}
	s.reconcile(ctx)
	return nil
}

func (s *Server) stepSync(ctx context.Context) error {
	if s.syncer == nil {
		return &core.Error{Code: core.CodeInvalidState, Message: "no ticket sync to step: no tracker is configured"}
	}
	_, err := s.syncer.Sync(ctx)
	return err
}

func (s *Server) stepWatch(ctx context.Context) error {
	if s.watch == nil {
		return &core.Error{Code: core.CodeInvalidState, Message: "no PR watch to step: pushes and pull requests are off"}
	}
	return s.watch.Poll(ctx)
}

// stepHandler answers POST /v1/step; only a stepped daemon has the route.
func (s *Server) stepHandler() http.HandlerFunc {
	return s.handle("step", read(func(ctx context.Context, req stepRequest) (any, error) {
		if err := s.Step(ctx, req.Loop); err != nil {
			return nil, err
		}
		return map[string]string{"loop": req.Loop}, nil
	}))
}
