package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
)

// CodeTokenForbidden refuses an operation that a run token may not perform.
const CodeTokenForbidden = "token_forbidden"

// agentOps are the operations a run token may perform. The agent commands of
// spec section 7.4 join them with their tickets.
var agentOps = []string{"whoami", "run.complete", "run.show", "run.logs", "ticket.show", "task.show"}

type runKey struct{}

// runOf returns the run of an agent's request.
func runOf(ctx context.Context) (runner.Run, bool) {
	run, ok := ctx.Value(runKey{}).(runner.Run)
	return run, ok
}

// asAgent checks a request made with a run token and returns it with the
// run's agent as its actor: the token must be valid, the operation one an
// agent may perform, and every ticket, task or run it names its run's own.
func (s *Server) asAgent(ctx context.Context, op string, raw json.RawMessage, token string) (runner.Run, json.RawMessage, error) {
	run, err := s.runs.ByToken(ctx, token)
	if err != nil {
		return runner.Run{}, nil, err
	}
	if !slices.Contains(agentOps, op) {
		return runner.Run{}, nil, &core.Error{Code: CodeTokenForbidden, Message: "a run token may not perform " + op}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return runner.Run{}, nil, errBadRequest{err}
	}
	scope := func(what, name string) error {
		return &core.Error{Code: runner.CodeTokenScope, Message: "the run token of " + run.Ticket + " may not act on " + what + " " + name}
	}
	for _, key := range []string{"ref", "ticket"} {
		var v string
		if json.Unmarshal(fields[key], &v) == nil && v != "" && v != run.Ticket {
			return runner.Run{}, nil, scope("ticket", v)
		}
	}
	var id string
	if json.Unmarshal(fields["id"], &id) == nil && id != "" {
		switch {
		case strings.HasPrefix(op, "task."):
			task, err := s.tasks.Get(ctx, id)
			if err != nil {
				return runner.Run{}, nil, err
			}
			if task.Ticket != run.Ticket {
				return runner.Run{}, nil, scope("task", id)
			}
		case strings.HasPrefix(op, "run.") && id != run.ID:
			return runner.Run{}, nil, scope("run", id)
		}
	}
	if _, ok := fields["actor"]; ok {
		fields["actor"], _ = json.Marshal(run.Agent())
		raw, _ = json.Marshal(fields)
	}
	return run, raw, nil
}
