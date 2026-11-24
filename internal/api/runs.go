package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/store"
)

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
