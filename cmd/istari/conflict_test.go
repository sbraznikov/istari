package main_test

import (
	"slices"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// task is a task as istari task list prints it.
type task struct{ ID, Ticket, Role, State string }

func (h *humanMerge) tasks(t *testing.T) []task {
	t.Helper()
	var all []task
	h.d.mustCLI(t, &all, "task", "list")
	var out []task
	for _, tk := range all {
		if tk.Ticket == h.ref {
			out = append(out, tk)
		}
	}
	return out
}

// returnedForConflict runs a watch pass and a pipeline pass and asserts the
// ticket is then in_progress with a ready implement task, got there by
// merge_failed (T10), and that the watch recorded the conflict.
func (h *humanMerge) returnedForConflict(t *testing.T) []task {
	t.Helper()
	h.d.step(t, "watch")
	h.d.step(t, "pipeline")
	if conflicts := h.d.eventTypes(t, h.ref, "vcs.conflicted"); len(conflicts) != 1 {
		t.Fatalf("the watch recorded %d conflicts, want 1", len(conflicts))
	}
	all := h.tasks(t)
	if st := h.state(t); st != "in_progress" || !slices.ContainsFunc(all, func(tk task) bool { return tk.Role == "implementer" && tk.State == "ready" }) {
		t.Fatalf("the ticket of a pull request in conflict is %s with %+v, want in_progress with a ready implement\n%s", st, all, h.d.stderr)
	}
	if tr := h.lastTrigger(t); tr != "merge_failed" {
		t.Fatalf("the ticket returned by %s, want merge_failed (T10)", tr)
	}
	return all
}

func (h *humanMerge) lastTrigger(t *testing.T) string {
	t.Helper()
	var events []struct{ Payload struct{ Trigger string } }
	h.d.mustCLI(t, &events, "tail", "--ticket", h.ref, "--type", "ticket.transitioned")
	if len(events) == 0 {
		t.Fatal("no transitions")
	}
	return events[len(events)-1].Payload.Trigger
}

func TestAPullRequestInConflictReturnsItsAwaitingApprovalTicketForRework(t *testing.T) {
	t.Parallel()
	h := recoveredWithPullRequest(t, "awaiting_approval")
	h.gh.SetMergeable(h.pr, new(false), "dirty") // another ticket merged into main
	h.returnedForConflict(t)
}

func TestAPullRequestInConflictReturnsItsInReviewTicketAndCancelsTheReview(t *testing.T) {
	t.Parallel()
	h := recoveredWithPullRequest(t, "in_review")
	h.gh.SetMergeable(h.pr, new(false), "dirty")
	for _, tk := range h.returnedForConflict(t) {
		if tk.Role == "reviewer" && (tk.State == "ready" || tk.State == "claimed" || tk.State == "blocked") {
			t.Fatalf("review task %s is %s, want the open one canceled and only the conflict round's waiting", tk.ID, tk.State)
		}
	}
}

func TestAPullRequestWhoseMergeableIsUnknownLeavesItsTicket(t *testing.T) {
	t.Parallel()
	h := recoveredWithPullRequest(t, "awaiting_approval")
	h.gh.SetMergeable(h.pr, nil, "unknown") // GitHub is still computing
	h.gh.SetHeadSHA(h.pr, "h1")
	h.gh.SetChecks("h1", githubtest.Check{Name: "test", Status: "in_progress"}) // so the poll leaves a trace
	h.d.step(t, "watch")
	if checks := h.d.eventTypes(t, h.ref, "vcs.checks"); len(checks) == 0 {
		t.Fatalf("the watch pass recorded no checks\n%s", h.d.stderr)
	}
	h.d.step(t, "pipeline")
	if conflicts := h.d.eventTypes(t, h.ref, "vcs.conflicted"); len(conflicts) != 0 {
		t.Fatalf("an unknown mergeable recorded %d conflicts", len(conflicts))
	}
	if st := h.state(t); st != "awaiting_approval" {
		t.Fatalf("with mergeable unknown the ticket is %s after a watch and a pipeline pass, want awaiting_approval", st)
	}
}
