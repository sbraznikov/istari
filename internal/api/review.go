package api

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// ReviewAnswer is what istari review submit answers: the findings code
// accepted and dropped. The verdict follows from them, by code.
type ReviewAnswer struct {
	Accepted int `json:"accepted"`
	Dropped  int `json:"dropped"`
}

// submitReview checks and records the reviewer's report and publishes it on
// the pull request (spec sections 7.6 and 7.7). It moves no ticket: the
// pipeline computes the verdict from the recorded report. A report outside
// its schema fails the run.
func (s *Server) submitReview(ctx context.Context, r ReviewSubmitRequest) (any, error) {
	run, isAgent := runOf(ctx)
	switch {
	case !isAgent:
		return nil, &core.Error{Code: runner.CodeTokenInvalid, Message: "istari review submit is for the reviewer: it needs the run token in ISTARI_TOKEN"}
	case run.Role != "reviewer":
		return nil, &core.Error{Code: CodeTokenForbidden, Message: "only the reviewer submits a review, not the " + run.Role}
	case s.reviews == nil || s.tracker == nil:
		return nil, &core.Error{Code: core.CodeInvalidState, Message: "reviewing is off: istari has no workspaces or no ticket tracker"}
	}
	checked, err := s.checkReview(ctx, run, r.Report)
	if early := (*notInReview)(nil); errors.As(err, &early) {
		return nil, s.yieldReview(ctx, run, early)
	}
	if err != nil {
		if _, rerr := s.runs.Reject(ctx, run.ID, err.Error()); rerr != nil {
			return nil, rerr
		}
		return nil, &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
	}
	if err := s.reviews.Record(ctx, run, checked); err != nil {
		return nil, err
	}
	if err := s.publishReview(ctx, run.Ticket, checked); err != nil {
		log.Printf("istari: review of %s: publishing on the pull request: %v", run.Ticket, err)
	}
	return ReviewAnswer{Accepted: len(checked.Accepted), Dropped: len(checked.Dropped)}, nil
}

func (s *Server) checkReview(ctx context.Context, run runner.Run, raw string) (runner.ReviewCheck, error) {
	res, err := runner.ParseReviewResult([]byte(raw))
	if err != nil {
		return runner.ReviewCheck{}, err
	}
	if tk, err := s.tickets.Get(ctx, run.Ticket); err != nil || tk.State != core.TicketInReview {
		if err != nil {
			return runner.ReviewCheck{}, err
		}
		return runner.ReviewCheck{}, &notInReview{ticket: run.Ticket, state: tk.State}
	}
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return runner.ReviewCheck{}, err
	}
	t, err := s.tracker.Get(ctx, ref)
	if err != nil {
		return runner.ReviewCheck{}, err
	}
	return s.reviews.Check(ctx, run, res, journal.StripWorkState(t.Body))
}

// notInReview refuses a review because of the ticket's state, not the
// report: the agent could not have made it acceptable.
type notInReview struct {
	ticket string
	state  core.TicketState
}

func (e *notInReview) Error() string {
	return fmt.Sprintf("review: %s is %s; a review is submitted in in_review", e.ticket, e.state)
}

// yieldReview ends a review run that came too early without a result and
// without spending an attempt (spec section 4.2), and notes it once per task
// in the ticket's journal.
func (s *Server) yieldReview(ctx context.Context, run runner.Run, early *notInReview) error {
	if _, err := s.runs.Yield(ctx, run.ID, early.Error()); err != nil {
		return err
	}
	if err := s.noteEarlyReview(ctx, run, early.state); err != nil {
		log.Printf("istari: review of %s: noting the early hand-out: %v", run.Ticket, err)
	}
	return &core.Error{Code: core.CodeInvalidState, Message: early.Error() +
		". Stop and end your run without a result, without istari complete: istari asks for the review again once the ticket is in review"}
}

// noteEarlyReview records that a review task was handed out before its
// ticket was in review, unless the journal already says so for this task.
func (s *Server) noteEarlyReview(ctx context.Context, run runner.Run, state core.TicketState) error {
	jt, ok := s.tracker.(journal.Tracker)
	if !ok {
		return nil
	}
	ref, err := tickets.ParseRef(run.Ticket)
	if err != nil {
		return err
	}
	j := journal.New(jt, s.store, s.clock)
	entries, err := j.Stored(ctx, ref)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Kind == journal.Note && e.ID == run.Task {
			return nil
		}
	}
	return j.Append(ctx, ref, journal.Entry{Kind: journal.Note, Actor: "system", ID: run.Task,
		Text: fmt.Sprintf("The review task %s was handed out before the ticket was in review: the ticket is %s. Its run ended without a result and spent no attempt.", run.Task, state)})
}

// publishReview posts the checked report on the ticket's pull request, the
// two axes apart; findings code dropped are not published.
func (s *Server) publishReview(ctx context.Context, ticket string, c runner.ReviewCheck) error {
	if s.vcs == nil || s.watch == nil {
		return nil
	}
	pr, err := s.watch.PullRequestOf(ctx, ticket)
	if err != nil {
		return err
	}
	return s.vcs.PublishReview(ctx, pr.Repo, pr.Number, c.Published())
}

// reviewOf tells how many findings code accepted in the report of a review
// task, from the ticket's journal; submitted is false while it has none.
func (s *Server) reviewOf(ctx context.Context, ticket, task string) (int, bool, error) {
	ref, err := tickets.ParseRef(ticket)
	if err != nil {
		return 0, false, err
	}
	entries, err := journal.New(nil, s.store, nil).Stored(ctx, ref)
	if err != nil {
		return 0, false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.Kind == journal.Review && e.Review.Task == task {
			return len(e.Review.Findings), true, nil
		}
	}
	return 0, false, nil
}
