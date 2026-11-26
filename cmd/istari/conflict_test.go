package main_test

import (
	"testing"
	"time"

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

// returnedForConflict waits until the ticket is in_progress with a ready
// implement task and asserts it got there by merge_failed (T10) and that
// the watch recorded the conflict.
func (h *humanMerge) returnedForConflict(t *testing.T) []task {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if h.state(t) == "in_progress" {
			for _, tk := range h.tasks(t) {
				if tk.Role == "implementer" && tk.State == "ready" {
					if conflicts := h.d.eventTypes(t, h.ref, "vcs.conflicted"); len(conflicts) != 1 {
						t.Fatalf("the watch recorded %d conflicts, want 1", len(conflicts))
					}
					if tr := h.lastTrigger(t); tr != "merge_failed" {
						t.Fatalf("the ticket returned by %s, want merge_failed (T10)", tr)
					}
					return h.tasks(t)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ticket of a pull request in conflict is %s with %+v, want in_progress with a ready implement\n%s", h.state(t), h.tasks(t), h.d.stderr)
		}
	}
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
	h.restart(t)
	h.returnedForConflict(t)
}

func TestAPullRequestInConflictReturnsItsInReviewTicketAndCancelsTheReview(t *testing.T) {
	t.Parallel()
	h := recoveredWithPullRequest(t, "in_review")
	h.gh.SetMergeable(h.pr, new(false), "dirty")
	h.restart(t)
	for _, tk := range h.returnedForConflict(t) {
		if tk.Role == "reviewer" && (tk.State == "ready" || tk.State == "claimed" || tk.State == "blocked") {
			t.Fatalf("review task %s is %s, want the open one canceled and only the conflict round's waiting", tk.ID, tk.State)
		}
	}
}

func TestAPullRequestWhoseMergeableIsUnknownLeavesItsTicket(t *testing.T) {
	t.Parallel()
	h := recoveredWithPullRequest(t, "awaiting_approval")
	h.stop(t) // no daemon polls a half-set pull request
	h.gh.SetMergeable(h.pr, nil, "unknown") // GitHub is still computing
	h.gh.SetHeadSHA(h.pr, "h1")
	h.gh.SetChecks("h1", githubtest.Check{Name: "test", Status: "in_progress"}) // so the poll leaves a trace
	h.start(t)
	// A slow poll on a loaded race job is no failure: wait as long as the test
	// may run, and leave a second to report.
	deadline, bounded := t.Deadline()
	deadline = deadline.Add(-time.Second)
	for len(h.d.eventTypes(t, h.ref, "vcs.checks")) == 0 {
		if bounded && time.Now().After(deadline) {
			t.Fatalf("the watch did not poll before the test deadline\n%s", h.d.stderr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Nothing to wait for: the ticket must stay through the reconciles after
	// the poll.
	for until := time.Now().Add(time.Second); time.Now().Before(until); time.Sleep(100 * time.Millisecond) {
		if conflicts := h.d.eventTypes(t, h.ref, "vcs.conflicted"); len(conflicts) != 0 {
			t.Fatalf("an unknown mergeable recorded %d conflicts", len(conflicts))
		}
		if st := h.state(t); st != "awaiting_approval" {
			t.Fatalf("with mergeable unknown the ticket is %s, want awaiting_approval", st)
		}
	}
}
