package pipeline

import (
	"context"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// mergeStage moves a ticket awaiting approval through the approve and merge
// stages (spec sections 4.3 and 4.8). In manual mode istari does neither: a
// human merges, and the PR watch's Merged finishes the ticket. Otherwise an
// approval of the head completes the approve task, and the merge task, once
// ready, merges the pull request with the configured method and fires T11.
// Only istari's own merge task fires T11 here: a round cut short before it
// made the merge task has merged nothing.
func (e *Engine) mergeStage(ctx context.Context, tk core.Ticket, r round, fire func(string) error) error {
	if m, ok := r.tasks[StageMerge]; ok && m.State == core.TaskCompleted {
		return fire("merged") // T11
	}
	ref, err := tickets.ParseRef(tk.Ref)
	if err != nil {
		return err
	}
	s, err := e.SettingsOf(ctx, ref.Repo)
	if err != nil {
		return err
	}
	if s.Merge.Mode == MergeManual {
		return nil
	}
	if a, ok := approveTask(r); ok && a.State == core.TaskReady {
		approved, err := e.approve(ctx, tk, a)
		if err != nil || !approved {
			return err
		}
	}
	m, ok := r.tasks[StageMerge]
	if !ok || e.opts.Merge == nil {
		return nil
	}
	if m, err = e.tasks.Get(ctx, m.ID); err != nil || m.State != core.TaskReady {
		return err // the approval is not given yet
	}
	if err := e.opts.Merge(ctx, tk.Ref, s.Merge.Method); err != nil {
		return err
	}
	if err := e.complete(ctx, m.ID); err != nil {
		return err
	}
	return fire("merged") // T11
}

// approveTask returns the round's approval given as a GitHub review.
func approveTask(r round) (core.Task, bool) {
	for _, t := range r.tasks {
		if t.Role == RoleGitHubReview {
			return t, true
		}
	}
	return core.Task{}, false
}

// approve completes the approve task when a CODEOWNERS member's review
// approves the pull request's current head; approved tells whether it did.
func (e *Engine) approve(ctx context.Context, tk core.Ticket, a core.Task) (approved bool, err error) {
	if e.opts.Approval == nil {
		return false, nil
	}
	by, err := e.opts.Approval(ctx, tk.Ref)
	if err != nil || by == "" {
		return false, err
	}
	return true, e.complete(ctx, a.ID)
}

// complete claims and completes a task code performs.
func (e *Engine) complete(ctx context.Context, id string) error {
	lease, err := e.tasks.Claim(ctx, e.opts.Actor, id)
	if err != nil {
		return err
	}
	return e.tasks.Complete(ctx, lease)
}
