package main_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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
	return newApprovalWaitWith(t, "", nil)
}

// newApprovalWaitWith is newApprovalWait with extra appended to the config
// and setup run on the fake GitHub before the daemon starts.
func newApprovalWaitWith(t *testing.T, extra string, setup func(gh *githubtest.Server)) *approvalWait {
	t.Helper()
	w := &approvalWait{}
	w.reviewed = recoverWith(t, silentReviewer, "awaiting_approval", "[defaults.merge]\nmethod = \"merge\"\n"+extra, func(gh *githubtest.Server, pr int) {
		if setup != nil {
			setup(gh)
		}
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

// gate is a task as istari gate list shows it.
type gate struct{ ID, Ticket, Role string }

// approveGate returns the approve gate of the ticket istari gate list shows,
// "" when it shows none.
func (w *approvalWait) approveGate(t *testing.T) string {
	t.Helper()
	var gates []gate
	w.d.mustCLI(t, &gates, "gate", "list")
	for _, g := range gates {
		if g.Ticket == w.ref.String() && g.Role == "github_review" {
			return g.ID
		}
	}
	return ""
}

// The fake seeds the pull request as bob, the gh user istari opened it as
// without a GitHub App; bob has write access and no trusted list is set, so
// bob is trusted (spec 4.5).
func TestWithoutAGitHubAppThePullRequestsTrustedAuthorApprovesWithGateApprove(t *testing.T) {
	t.Parallel()
	w := newApprovalWait(t)
	w.gh.SetPermission("bob", "write")
	id := w.approveGate(t)
	if id == "" {
		t.Fatalf("gate list shows no approve gate of %s awaiting approval\n%s", w.ref, w.d.stderr)
	}
	w.d.mustCLI(t, nil, "gate", "approve", id, "--actor", "human:bob")
	w.d.step(t, "pipeline") // the approval at h1 completes approve; istari merges
	if pr := w.gh.Issue(w.pr); !pr.Merged {
		t.Fatalf("after the author's gate approve the pull request is not merged\n%s", w.d.stderr)
	}
	if st := w.state(t, w.ref.String()); st != "done" {
		t.Fatalf("the ticket is %s, want done (T11)\n%s", st, w.d.stderr)
	}
	var approved []struct {
		Actor   string
		Payload struct{ By, Head string }
	}
	w.d.mustCLI(t, &approved, "tail", "--ticket", w.ref.String(), "--type", "gate.approved")
	if len(approved) != 1 || approved[0].Actor != "human:bob" || approved[0].Payload.By != "bob" || approved[0].Payload.Head != "h1" {
		t.Fatalf("gate.approved events = %+v, want one by bob at h1", approved)
	}
	var notes []struct{ Payload struct{ Text string } }
	w.d.mustCLI(t, &notes, "tail", "--ticket", w.ref.String(), "--type", "journal.note")
	if !slices.ContainsFunc(notes, func(n struct{ Payload struct{ Text string } }) bool {
		return strings.Contains(n.Payload.Text, "human:bob approved") && strings.Contains(n.Payload.Text, "h1")
	}) {
		t.Fatalf("journal notes = %+v, want one naming bob and h1", notes)
	}
}

func TestAPushAfterTheAuthorsGateApprovalVoidsItAndTheGateShowsAgain(t *testing.T) {
	t.Parallel()
	w := newApprovalWait(t)
	w.gh.SetPermission("bob", "write")
	id := w.approveGate(t)
	w.d.mustCLI(t, nil, "gate", "approve", id, "--actor", "human:bob") // at h1
	w.gh.SetHeadSHA(w.pr, "h2")
	w.d.step(t, "watch")
	w.d.step(t, "pipeline")
	if pr := w.gh.Issue(w.pr); pr.Merged {
		t.Fatal("an approval of h1 merged the pull request at h2")
	}
	if st := w.state(t, w.ref.String()); st != "awaiting_approval" {
		t.Fatalf("the ticket is %s, want awaiting_approval", st)
	}
	if again := w.approveGate(t); again != id {
		t.Fatalf("gate list shows %q after the push, want the approve gate %s again", again, id)
	}
}

// A refused gate approve: the exit is not 0 and stderr names code and what.
func (w *approvalWait) refusedApprove(t *testing.T, actor string, want ...string) {
	t.Helper()
	id := w.approveGate(t)
	if id == "" {
		t.Fatalf("gate list shows no approve gate of %s", w.ref)
	}
	_, stderr, code := w.d.cli(t, "gate", "approve", id, "--actor", actor)
	if code == 0 {
		t.Fatalf("gate approve by %s passed, want refused", actor)
	}
	for _, s := range want {
		if !strings.Contains(stderr, s) {
			t.Fatalf("gate approve by %s: %q, want it to contain %q", actor, stderr, s)
		}
	}
	if w.d.step(t, "pipeline"); w.gh.Issue(w.pr).Merged {
		t.Fatalf("a refused gate approve by %s merged the pull request", actor)
	}
}

func TestAGateApproveByAMemberNotTrustedIsRefusedNamingWhoMayApprove(t *testing.T) {
	t.Parallel()
	w := newApprovalWait(t) // bob reads only: not trusted without a trusted list
	w.refusedApprove(t, "human:bob", "not_trusted", "trusted are the members with write access to "+githubtest.Repo)
}

func TestAGateApproveByATrustedMemberWhoIsNotTheAuthorPointsToAGitHubReview(t *testing.T) {
	t.Parallel()
	w := newApprovalWait(t)
	w.gh.SetPermission("alex", "write")
	w.refusedApprove(t, "human:alex", "bob opened the pull request, not alex", "approve it there")
}

func TestWithAGitHubAppGateApproveOnTheApproveTaskIsRefusedPointingToTheGitHubReview(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	app := fmt.Sprintf("\n[github_app]\napp_id = 4242\nprivate_key = %q\n", pemPath)
	w := newApprovalWaitWith(t, app, func(gh *githubtest.Server) { gh.EnableApp(4242, &key.PublicKey, 77) })
	w.gh.SetPermission("bob", "write")
	if id := w.approveGate(t); id != "" {
		t.Fatalf("with a GitHub App gate list shows the approve gate %s, want none", id)
	}
	var tasks []gate
	w.d.mustCLI(t, &tasks, "task", "list")
	id := ""
	for _, task := range tasks {
		if task.Ticket == w.ref.String() && task.Role == "github_review" {
			id = task.ID
		}
	}
	_, stderr, code := w.d.cli(t, "gate", "approve", id, "--actor", "human:bob")
	if code == 0 || !strings.Contains(stderr, "GitHub review from a member listed in CODEOWNERS") {
		t.Fatalf("gate approve with a GitHub App: exit %d, %q; want refused, pointing to the GitHub review", code, stderr)
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
