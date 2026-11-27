package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
)

// resume makes an unfinished ticket's work go on now (spec sections 4.1 and
// 4.6, istari ticket resume): a paused hold is lifted, and a claimed task
// whose run is gone goes back to ready without waiting for its lease. The
// next run starts from the ticket, its journal and its branch, with the main
// branch merged in. Tasks whose attempts ran out are istari ticket retry's.
func (s *Server) resume(ctx context.Context, actor, ref string) (core.Ticket, error) {
	if err := requireHuman(actor, "a ticket is resumed"); err != nil {
		return core.Ticket{}, err
	}
	tk, err := s.tickets.Get(ctx, ref)
	if err != nil {
		return core.Ticket{}, err
	}
	if tk.State == core.TicketDone || tk.State == core.TicketCanceled {
		return core.Ticket{}, &core.Error{Code: core.CodeInvalidState, Message: ref + " is " + string(tk.State) + ": there is nothing to resume"}
	}
	all, err := s.tasks.List(ctx)
	if err != nil {
		return core.Ticket{}, err
	}
	for _, task := range all {
		if task.Ticket != ref {
			continue
		}
		if task.State == core.TaskClaimed {
			if s.runner != nil {
				if run, ok := s.runner.Working(ctx, task.ID); ok {
					return core.Ticket{}, &core.Error{Code: core.CodeInvalidState, Message: "task " + task.ID + " is running in run " + run + "; stop it with istari run stop first"}
				}
			}
			if err := s.tasks.Revoke(ctx, actor, task.ID); err != nil {
				return core.Ticket{}, err
			}
		}
	}
	if tk.Hold == "paused" {
		return s.tickets.ClearHold(ctx, actor, ref, "paused")
	}
	return tk, nil
}

// unholdBudget is a human's decision on an exceeded budget (spec section 7.5,
// istari ticket unhold). With the runner off no budget is configured here, so
// nothing counts as spent.
func (s *Server) unholdBudget(ctx context.Context, actor, ref string, anyway bool) (core.Ticket, error) {
	if err := requireHuman(actor, "a budget hold is lifted"); err != nil {
		return core.Ticket{}, err
	}
	r := s.runner
	if r == nil {
		r = runner.New(s.store, s.tasks, s.tickets, s.spaces, runner.RunnerOptions{})
	}
	return r.LiftBudgetHold(ctx, actor, ref, anyway)
}

// answer records a human's answer to an open question of the ticket (spec
// section 4.6, istari ticket answer); the answer to the last one lifts
// needs_input.
func (s *Server) answer(ctx context.Context, r TicketAnswerRequest) (core.Ticket, error) {
	if err := requireHuman(r.Actor, "a question is answered"); err != nil {
		return core.Ticket{}, err
	}
	if strings.TrimSpace(r.Text) == "" {
		return core.Ticket{}, &core.Error{Code: core.CodeInvalidTicket, Message: "an answer needs a text"}
	}
	ref, err := tickets.ParseRef(r.Ref)
	if err != nil {
		return core.Ticket{}, &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
	}
	if _, err := s.tickets.Get(ctx, r.Ref); err != nil {
		return core.Ticket{}, err
	}
	_, err = s.journal.Answer(ctx, s.tickets, ref, journal.Reply{Question: r.Question, Actor: r.Actor, Text: r.Text})
	if errors.Is(err, journal.ErrNoOpenQuestion) {
		return core.Ticket{}, &core.Error{Code: core.CodeInvalidState, Message: fmt.Sprintf("ticket %s has no open question", r.Ref)}
	}
	if err != nil {
		return core.Ticket{}, err
	}
	return s.tickets.Get(ctx, r.Ref)
}

// recoverTickets rebuilds what istari lost with its database from the
// tracker (spec section 4.6, istari recover): the tickets istari worked on,
// with their state, hold and journal, and the pull requests their journals
// name, which are watched again.
func (s *Server) recoverTickets(ctx context.Context, actor string) (tickets.SyncReport, error) {
	if err := requireHuman(actor, "istari recover is run"); err != nil {
		return tickets.SyncReport{}, err
	}
	if s.syncer == nil {
		return tickets.SyncReport{}, &core.Error{Code: CodeSyncDisabled, Message: "no project has a ticket tracker"}
	}
	jt, ok := s.tracker.(journal.Tracker)
	if !ok {
		return tickets.SyncReport{}, &core.Error{Code: CodeSyncDisabled, Message: "the ticket tracker keeps no journal"}
	}
	j := journal.New(jt, s.store, s.clock)
	return s.syncer.Recover(ctx, func(ctx context.Context, ref tickets.Ref) error {
		if err := j.Restore(ctx, ref); err != nil {
			return err
		}
		if s.watch == nil {
			return nil
		}
		entries, err := j.Stored(ctx, ref)
		if err != nil {
			return err
		}
		pr, ok := vcs.PullRequestAt(journal.Summarize(entries).PR)
		if !ok {
			return nil // no pull request yet
		}
		return s.watch.Track(ctx, ref.String(), ref.Repo, pr)
	})
}

// requireHuman refuses an action of anyone but a human.
func requireHuman(actor, action string) error {
	if !strings.HasPrefix(actor, "human:") {
		return &core.Error{Code: core.CodeHumanOnly, Message: action + " by a human, not " + actor}
	}
	return nil
}
