package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/sbraznikov/istari/internal/core"
)

// The loops a stepped daemon runs one pass of per POST /v1/step (spec 3.1).
const (
	LoopScheduler = "scheduler" // lease expiry and due timers
	LoopPipeline  = "pipeline"  // the pipeline engine, then the worktrees
	LoopSync      = "sync"      // the ticket sync
	LoopWatch     = "watch"     // the PR watch
)

type stepRequest struct {
	Loop string `json:"loop"`
}

func (r stepRequest) validate() error {
	switch r.Loop {
	case LoopScheduler, LoopPipeline, LoopSync, LoopWatch:
		return nil
	}
	return fmt.Errorf("loop %q is not %s, %s, %s or %s", r.Loop, LoopScheduler, LoopPipeline, LoopSync, LoopWatch)
}

// steps serializes the passes of a stepped daemon: as the timed loops do,
// no loop runs two passes at once.
type steps struct{ mu sync.Mutex }

// Step runs one pass of loop and returns when it has finished. A loop the
// daemon has not configured, such as sync without a tracker, is refused.
func (s *Server) Step(ctx context.Context, loop string) error {
	s.steps.mu.Lock()
	defer s.steps.mu.Unlock()
	switch loop {
	case LoopScheduler:
		return s.sched.Step(ctx)
	case LoopPipeline:
		if len(s.repos) == 0 {
			return &core.Error{Code: core.CodeInvalidState, Message: "no pipeline to step: no project has a repository"}
		}
		s.reconcile(ctx)
		return nil
	case LoopSync:
		if s.syncer == nil {
			return &core.Error{Code: core.CodeInvalidState, Message: "no ticket sync to step: no tracker is configured"}
		}
		_, err := s.syncer.Sync(ctx)
		return err
	case LoopWatch:
		if s.watch == nil {
			return &core.Error{Code: core.CodeInvalidState, Message: "no PR watch to step: pushes and pull requests are off"}
		}
		return s.watch.Poll(ctx)
	}
	return errBadRequest{stepRequest{Loop: loop}.validate()}
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
