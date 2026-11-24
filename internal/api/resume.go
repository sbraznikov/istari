package api

import (
	"context"
	"strconv"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
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
	if !strings.HasPrefix(actor, "human:") {
		return core.Ticket{}, &core.Error{Code: core.CodeHumanOnly, Message: "a ticket is resumed by a human, not " + actor}
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
		switch task.State {
		case core.TaskClaimed:
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

// recoverTickets rebuilds what istari lost with its database from the
// tracker (spec section 4.6, istari recover): the tickets istari worked on,
// with their state, hold and journal, and the pull requests their journals
// name, which are watched again.
func (s *Server) recoverTickets(ctx context.Context, actor string) (tickets.SyncReport, error) {
	if !strings.HasPrefix(actor, "human:") {
		return tickets.SyncReport{}, &core.Error{Code: core.CodeHumanOnly, Message: "istari recover is run by a human, not " + actor}
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
		url := journal.Summarize(entries).PR
		n, err := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
		if url == "" || err != nil {
			return nil // no pull request yet
		}
		return s.watch.Track(ctx, ref.String(), ref.Repo, vcs.Opened{Number: n, URL: url})
	})
}
