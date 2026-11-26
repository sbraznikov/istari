package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/github/ratelimit"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
)

// CodeTrackerUnavailable answers a submit whose tracker write met an outage:
// the run ends without spending an attempt (spec section 4.2).
const CodeTrackerUnavailable = "tracker_unavailable"

// rejectOrYield ends a submit that code could not take. A tracker outage is
// not the agent's fault: the run yields and its task is handed out again
// without spending an attempt; nothing of the result is kept, the next run
// does the work again. A read that GitHub refused for the rate limit is
// "later": it marks nothing, and the agent submits the same result again
// (#208). Any other refusal rejects the run (spec section 4.2).
func (s *Server) rejectOrYield(ctx context.Context, run runner.Run, err error) error {
	if errors.Is(err, tickets.ErrUnavailable) {
		if _, yerr := s.runs.Yield(ctx, run.ID, err.Error()); yerr != nil {
			return yerr
		}
		log.Printf("istari: %s: %v; tried again when task %s is handed out next", run.Ticket, err, run.Task)
		return &core.Error{Code: CodeTrackerUnavailable, Message: err.Error() +
			". This costs no attempt, and nothing of your result is kept: the next run does the work again. Stop and end your run without istari complete"}
	}
	if errors.Is(err, ratelimit.ErrRateLimited) {
		return &core.Error{Code: CodeRateLimited, Message: err.Error() + "; GitHub refuses for the rate limit: submit the same result again later"}
	}
	if _, rerr := s.runs.Reject(ctx, run.ID, err.Error()); rerr != nil {
		return rerr
	}
	return &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
}

// RunLogs is a piece of a run's transcript.
type RunLogs struct {
	Lines []json.RawMessage    `json:"lines"`
	Next  runner.TranscriptPos `json:"next"`  // ask again from here for what follows
	Ended bool                 `json:"ended"` // the run has ended: nothing will follow
}

// runLogs reads the run's transcript after a position.
func (s *Server) runLogs(ctx context.Context, r RunLogsRequest) (RunLogs, error) {
	run, err := s.runs.Get(ctx, r.ID)
	if err != nil {
		return RunLogs{}, err
	}
	ended := run.State == runner.RunEnded // read before the lines: none can follow these
	lines, next, err := runner.ReadTranscript(run.Transcript, r.After)
	if err != nil {
		return RunLogs{}, err
	}
	return RunLogs{Lines: lines, Next: next, Ended: ended}, nil
}

// Whoami is what an agent learns of itself (spec section 7.2).
type Whoami struct {
	Run            string    `json:"run"`
	Profile        string    `json:"profile"`
	Role           string    `json:"role"`
	Ticket         string    `json:"ticket"`
	Task           string    `json:"task"`
	Permissions    []string  `json:"permissions"`
	LeaseUntil     time.Time `json:"lease_until"`
	LeaseRemaining string    `json:"lease_remaining"`
	BudgetUSD      float64   `json:"budget_usd"` // 0: none
	SpentUSD       float64   `json:"spent_usd"`
	BudgetLeftUSD  float64   `json:"budget_left_usd,omitempty"`
}

func (s *Server) whoami(ctx context.Context) (Whoami, error) {
	run, ok := runOf(ctx)
	if !ok {
		return Whoami{}, &core.Error{Code: runner.CodeTokenInvalid, Message: "istari whoami is for an agent: it needs the run token in ISTARI_TOKEN"}
	}
	task, err := s.tasks.Get(ctx, run.Task)
	if err != nil {
		return Whoami{}, err
	}
	w := Whoami{Run: run.ID, Profile: run.Profile, Role: run.Role, Ticket: run.Ticket, Task: run.Task,
		LeaseUntil: task.LeaseUntil, LeaseRemaining: task.LeaseUntil.Sub(s.clock()).Round(time.Second).String(), BudgetUSD: run.BudgetUSD}
	// Least privilege (spec section 11): only the implementer pushes, and
	// only its own branch; the others read.
	w.Permissions = []string{"act on ticket " + run.Ticket, "work in " + run.Dir}
	if run.Role == "implementer" && s.spaces != nil {
		if ws, err := s.spaces.Get(ctx, run.Ticket); err == nil && ws.Branch != "" {
			w.Permissions = append(w.Permissions, "push to "+ws.Branch)
		}
	} else {
		w.Permissions = append(w.Permissions, "read the repository; no push")
	}
	events, err := s.store.Read(ctx, "", store.Filter{Ticket: run.Ticket, TypePrefix: runner.EventRunUsage}, 0)
	if err != nil {
		return Whoami{}, err
	}
	for _, e := range events {
		var p struct {
			Run   string `json:"run"`
			Usage struct {
				CostUSD float64 `json:"cost_usd"`
			} `json:"usage"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Run == run.ID {
			w.SpentUSD += p.Usage.CostUSD
		}
	}
	if w.BudgetUSD > 0 {
		w.BudgetLeftUSD = max(0, w.BudgetUSD-w.SpentUSD)
	}
	return w, nil
}
