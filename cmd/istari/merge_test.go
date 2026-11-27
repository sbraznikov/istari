package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// cleanReviewer submits a report without findings.
const cleanReviewer = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (fake)"; exit 0; }
istari=%[1]q
log=%[2]q
if [ "$ISTARI_ROLE" = reviewer ]; then
  "$istari" review submit --file - >> "$log" 2>&1 <<'REPORT'
` + validReport + `
REPORT
  "$istari" complete "reviewed" >> "$log" 2>&1
fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","num_turns":1,"result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}'
`

// approvalWait is a ticket recovered in awaiting_approval, whose pull
// request is at head h1 in a repository kim owns by CODEOWNERS, and a ticket
// that needs it, on a stepped daemon whose merge method is "merge".
type approvalWait struct {
	reviewed
	branch string
	depRef string
}

func newApprovalWait(t *testing.T) *approvalWait {
	t.Helper()
	w := &approvalWait{}
	w.reviewed = recoverWith(t, silentReviewer, "awaiting_approval", "[defaults.merge]\nmethod = \"merge\"\n", func(gh *githubtest.Server, pr int) {
		gh.SetFile(".github/CODEOWNERS", "* @kim\n")
		gh.SetHeadSHA(pr, "h1")
		w.branch = gh.Issue(pr).Branch
		need := strings.Split(w.branch, "/")[1]
		body := strings.Replace(strings.SplitN(conforming, "\n", 2)[1], "- Needs: none", "- Needs: #"+need, 1)
		w.depRef = fmt.Sprintf("gh:%s#%d", githubtest.Repo, gh.Seed("Use rate limits", body, "alex"))
	})
	w.d.mustCLI(t, nil, "sync")
	w.d.step(t, "pipeline") // the recovered state's tasks and worktree
	if st := w.state(t, w.ref.String()); st != "awaiting_approval" {
		t.Fatalf("the recovered ticket is %s, want awaiting_approval\n%s", st, w.d.stderr)
	}
	return w
}

func (w *approvalWait) state(t *testing.T, ref string) string {
	t.Helper()
	var tk struct{ State string }
	w.d.mustCLI(t, &tk, "ticket", "show", ref)
	return tk.State
}

// worktree returns the ticket's workspace as istari lists it.
func (w *approvalWait) worktree(t *testing.T) (state, dir, branch string) {
	t.Helper()
	var list []struct{ Ticket, State, Branch, Dir string }
	w.d.mustCLI(t, &list, "workspace", "list")
	for _, ws := range list {
		if ws.Ticket == w.ref.String() {
			return ws.State, ws.Dir, ws.Branch
		}
	}
	t.Fatalf("workspace list = %+v, without %s", list, w.ref)
	return "", "", ""
}

func TestAfterApprovalAnOwnersApprovalOfTheHeadMergesAndFinishesTheTicket(t *testing.T) {
	t.Parallel()
	w := newApprovalWait(t)
	_, dir, branch := w.worktree(t)
	if !exists(dir) {
		t.Fatalf("the awaiting ticket has no worktree at %s", dir)
	}
	w.gh.AddReview(w.pr, "kim", "APPROVED")
	w.d.step(t, "watch")    // the watch sees the approval
	w.d.step(t, "pipeline") // istari merges, the ticket is done, its worktree goes
	if pr := w.gh.Issue(w.pr); !pr.Merged || pr.MergeMethod != "merge" {
		t.Fatalf("the pull request is merged %v with %q, want merged with the configured merge\n%s", pr.Merged, pr.MergeMethod, w.d.stderr)
	}
	if st := w.state(t, w.ref.String()); st != "done" {
		t.Fatalf("after istari's merge the ticket is %s, want done (T11)\n%s", st, w.d.stderr)
	}
	if !slices.Contains(w.gh.DeletedBranches(), w.branch) {
		t.Fatalf("deleted branches = %v, want the remote %s", w.gh.DeletedBranches(), w.branch)
	}
	if state, _, _ := w.worktree(t); state != "removed" || exists(dir) {
		t.Fatalf("the worktree is %s, its directory there: %v; want removed", state, exists(dir))
	}
	clone := filepath.Join(w.d.dataDir, "repos", "acme", "web.git")
	if out, err := exec.Command("git", "-C", clone, "branch", "--list", branch).Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the local branch %s: %q, %v; want gone", branch, out, err)
	}
	w.d.step(t, "pipeline") // the dependent's need is done: it starts (T3)
	if st := w.state(t, w.depRef); st != "planning" {
		t.Fatalf("the ticket that needs the merged one is %s, want planning\n%s", st, w.d.stderr)
	}
	w.d.mustCLI(t, nil, "sync")
	if i := w.gh.Issue(w.ref.Number); i.State != "closed" || i.StateReason != "completed" {
		t.Fatalf("the issue is %s/%s, want closed as completed", i.State, i.StateReason)
	}
}

func TestAnApprovalOfAnOlderHeadMergesNothing(t *testing.T) {
	t.Parallel()
	w := newApprovalWait(t)
	w.gh.AddReview(w.pr, "kim", "APPROVED") // of h1
	w.gh.SetHeadSHA(w.pr, "h2")
	w.d.step(t, "watch")
	w.d.step(t, "pipeline")
	if pr := w.gh.Issue(w.pr); pr.Merged {
		t.Fatal("a review of an older head merged the pull request")
	}
	if st := w.state(t, w.ref.String()); st != "awaiting_approval" {
		t.Fatalf("the ticket is %s, want awaiting_approval", st)
	}
}

func TestAMergeGitHubRefusesReturnsTheTicketToInProgress(t *testing.T) {
	t.Parallel()
	w := newApprovalWait(t)
	w.gh.RefuseMerge(w.pr, 405, `Required status check "test" is expected.`)
	w.gh.AddReview(w.pr, "kim", "APPROVED")
	w.d.step(t, "watch")
	w.d.step(t, "pipeline")
	if st := w.state(t, w.ref.String()); st != "in_progress" {
		t.Fatalf("after a refused merge the ticket is %s, want in_progress (T10)\n%s", st, w.d.stderr)
	}
	var notes []struct{ Payload struct{ Text string } }
	w.d.mustCLI(t, &notes, "tail", "--ticket", w.ref.String(), "--type", "journal.note")
	if !slices.ContainsFunc(notes, func(n struct{ Payload struct{ Text string } }) bool {
		return strings.Contains(n.Payload.Text, `Required status check "test" is expected.`)
	}) {
		t.Fatalf("journal notes = %+v, want one naming GitHub's reason", notes)
	}
}

func TestInAutoModeAPullRequestThatMeetsEveryConditionMergesWithoutAHumanReview(t *testing.T) {
	t.Parallel()
	r := recoverWith(t, cleanReviewer, "in_review", "[defaults.merge]\nmode = \"auto\"\n", func(gh *githubtest.Server, pr int) {
		gh.SetHeadSHA(pr, "h1")
		gh.SetChecks("h1", githubtest.Check{Name: "test", Status: "completed", Conclusion: "success"})
		gh.SetPullFiles(pr, githubtest.File{Path: "feature.txt", Additions: 1})
	})
	r.gh.HumanAddLabel(r.ref.Number, "alex", "size:S")
	r.d.mustCLI(t, nil, "sync")
	r.d.step(t, "watch")    // green CI
	r.d.step(t, "pipeline") // the review task
	for deadline := time.Now().Add(30 * time.Second); len(r.d.eventTypes(t, r.ref.String(), "task.completed")) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			out, _ := os.ReadFile(r.log)
			t.Fatalf("the review did not complete\nagent log:\n%s\ndaemon:\n%s", out, r.d.stderr)
		}
	}
	r.d.step(t, "pipeline") // T8
	r.d.step(t, "pipeline") // auto: approve skipped, merged
	var tk struct{ State, Size string }
	r.d.mustCLI(t, &tk, "ticket", "show", r.ref.String())
	if tk.State != "done" || !r.gh.Issue(r.pr).Merged {
		events, _, _ := r.d.cli(t, "tail", "--ticket", r.ref.String())
		t.Fatalf("the ticket (size %q) is %s, merged %v; want merged and done without a human\nevents:\n%s\ndaemon:\n%s", tk.Size, tk.State, r.gh.Issue(r.pr).Merged, events, r.d.stderr)
	}
	if reviews := r.gh.Issue(r.pr).Reviews; slices.ContainsFunc(reviews, func(rv githubtest.Review) bool { return rv.State == "APPROVED" }) {
		t.Fatalf("reviews = %+v, want no approval", reviews)
	}
}
