package api

import (
	"context"
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
// its schema fails the run, unless a later report of the run is accepted.
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
	if err != nil {
		if _, rerr := s.runs.Reject(ctx, run.ID, err.Error()); rerr != nil {
			return nil, rerr
		}
		return nil, &core.Error{Code: core.CodeInvalidTicket, Message: err.Error()}
	}
	if err := s.reviews.Record(ctx, run, checked); err != nil {
		return nil, err
	}
	if _, err := s.runs.Accept(ctx, run.ID); err != nil {
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
		return runner.ReviewCheck{}, fmt.Errorf("review: %s is %s; a review is submitted in in_review", run.Ticket, tk.State)
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
