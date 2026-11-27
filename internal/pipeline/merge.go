package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
)

// Refusal is one condition of auto mode (spec 4.8) a pull request fails:
// the condition's name and why.
type Refusal struct {
	Condition string `json:"condition"`
	Reason    string `json:"reason"`
}

// The conditions of auto mode, as a Refusal names them.
const (
	CondSize         = "size"
	CondReview       = "review"
	CondCI           = "ci"
	CondIrreversible = "irreversible_paths"
	CondMergeDanger  = "merge_danger"
	CondLines        = "lines"
	CondFiles        = "files"
	CondAssumptions  = "assumptions"
)

// EventAutoMergeRefused records a pull request auto mode leaves to a human,
// once per head, with each condition it fails.
const EventAutoMergeRefused = "merge.auto_refused"

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
		approved, err := e.approve(ctx, tk, r, s.Merge, a)
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
// approves the pull request's current head, or in auto mode when the pull
// request meets every condition of spec 4.8, which skips the human. A pull
// request auto mode refuses waits for a human's approval as in
// after_approval. approved tells whether the task was completed.
func (e *Engine) approve(ctx context.Context, tk core.Ticket, r round, s MergeSettings, a core.Task) (approved bool, err error) {
	if s.Mode == MergeAuto {
		head, refused, err := e.autoRefusals(ctx, tk, r, s)
		if err != nil {
			return false, err
		}
		if len(refused) == 0 {
			return true, e.complete(ctx, a.ID)
		}
		if err := e.refuseAuto(ctx, tk.Ref, head, refused); err != nil {
			return false, err
		}
	}
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

// autoRefusals checks the conditions of auto mode at once (spec 4.8) and
// returns the head they were checked at and each one the pull request fails.
func (e *Engine) autoRefusals(ctx context.Context, tk core.Ticket, r round, s MergeSettings) (head string, out []Refusal, err error) {
	refuse := func(cond, format string, args ...any) {
		out = append(out, Refusal{Condition: cond, Reason: fmt.Sprintf(format, args...)})
	}
	sizes := core.Sizes()
	switch {
	case tk.Size == "":
		refuse(CondSize, "the ticket has no size")
	case slices.Index(sizes, tk.Size) > slices.Index(sizes, s.AutoMaxSize):
		refuse(CondSize, "the ticket is %s, larger than auto_max_size %s", tk.Size, s.AutoMaxSize)
	}
	switch review, ok := r.tasks["review"]; {
	case !ok:
		refuse(CondReview, "the round has no agent review")
	case e.opts.Review != nil:
		findings, _, err := e.opts.Review(ctx, tk.Ref, review.ID)
		if err != nil {
			return "", nil, err
		}
		if findings > 0 {
			refuse(CondReview, "the review left open findings (%d)", findings)
		}
	}
	if e.opts.Checks == nil {
		refuse(CondCI, "CI is not watched")
	} else if state, _ := e.opts.Checks(ctx, tk.Ref); state != "success" {
		refuse(CondCI, "CI is %q, not green", state)
	}
	if e.opts.Changes == nil {
		refuse(CondIrreversible, "the diff is not read")
		return "", out, nil
	}
	changes, err := e.opts.Changes(ctx, tk.Ref)
	if err != nil {
		return "", nil, err
	}
	for _, p := range changes.Paths {
		for _, pattern := range s.IrreversiblePaths {
			if globMatch(strings.Split(pattern, "/"), strings.Split(p, "/")) {
				refuse(CondIrreversible, "the diff touches %s, an irreversible path (%s)", p, pattern)
				break
			}
		}
	}
	if danger := strings.ToLower(changes.MergeDanger); strings.Contains(danger, "one-way") || strings.Contains(danger, "one way") {
		refuse(CondMergeDanger, "the Merge Danger section calls the change a one-way door")
	}
	if limit := s.AutoMaxLines[tk.Size]; limit > 0 && changes.Lines > limit {
		refuse(CondLines, "the diff changes %d lines, more than the %d of size %s", changes.Lines, limit, tk.Size)
	}
	if limit := s.AutoMaxFiles[tk.Size]; limit > 0 && changes.Files > limit {
		refuse(CondFiles, "the diff changes %d files, more than the %d of size %s", changes.Files, limit, tk.Size)
	}
	n, err := e.assumptions(ctx, tk.Ref)
	if err != nil {
		return "", nil, err
	}
	if n > 0 {
		refuse(CondAssumptions, "assumptions recorded during the work await a human's verification (%d)", n)
	}
	return changes.Head, out, nil
}

// assumptions counts the assumptions in the ticket's journal: none is
// verified by a human yet, so each blocks auto-merge (spec 7.7).
func (e *Engine) assumptions(ctx context.Context, refText string) (int, error) {
	entries, err := e.entries(ctx, refText)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, en := range entries {
		if en.Kind == journal.Assumption {
			n++
		}
	}
	return n, nil
}

// entries returns the ticket's journal; none without a journal.
func (e *Engine) entries(ctx context.Context, refText string) ([]journal.Entry, error) {
	if e.opts.Journal == nil {
		return nil, nil
	}
	ref, err := tickets.ParseRef(refText)
	if err != nil {
		return nil, err
	}
	return e.opts.Journal.Stored(ctx, ref)
}

// autoRefusedAt starts the journal note on a pull request auto mode leaves
// to a human at head.
func autoRefusedAt(head string) string {
	return fmt.Sprintf("auto-merge refused at %s: ", head)
}

// refuseAuto notes in the journal, and records as an event, each condition
// the pull request fails, once per head.
func (e *Engine) refuseAuto(ctx context.Context, refText, head string, refused []Refusal) error {
	entries, err := e.entries(ctx, refText)
	if err != nil || e.opts.Journal == nil {
		return err
	}
	if slices.ContainsFunc(entries, func(en journal.Entry) bool {
		return en.Kind == journal.Note && strings.HasPrefix(en.Text, autoRefusedAt(head))
	}) {
		return nil
	}
	reasons := make([]string, len(refused))
	for i, f := range refused {
		reasons[i] = f.Reason
	}
	text := autoRefusedAt(head) + strings.Join(reasons, "; ") + ". The pull request waits for a human's approval as in after_approval (spec 4.8)"
	if err := e.noteOnce(ctx, refText, text); err != nil {
		return err
	}
	if e.opts.Log == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"head": head, "refused": refused})
	if err != nil {
		return err
	}
	_, err = e.opts.Log.Append(ctx, store.Event{Type: EventAutoMergeRefused, Actor: e.opts.Actor, Ticket: refText, Payload: payload})
	return err
}
