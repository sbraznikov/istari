package vcs_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/vcs"
)

var ctx = context.Background()

func appFixture(t *testing.T) (*githubtest.Server, *vcs.AppTokens) {
	t.Helper()
	gh := githubtest.New(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh.EnableApp(4242, &key.PublicKey, 77)
	pemPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens, err := vcs.NewAppTokens(4242, pemPath, gh.URL)
	if err != nil {
		t.Fatal(err)
	}
	return gh, tokens
}

func TestTheAppGetsAnInstallationTokenAndKeepsItUntilItExpires(t *testing.T) {
	gh, tokens := appFixture(t)
	first, err := tokens.Token(ctx, githubtest.Repo)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if second, _ := tokens.Token(ctx, githubtest.Repo); second != first {
		t.Fatalf("a second call got %q, want the cached %q", second, first)
	}
	if issued := gh.InstallationTokens(); len(issued) != 1 || issued[0] != first {
		t.Fatalf("issued = %v, want one token, %q", issued, first)
	}
	// The token works on the API.
	c := vcs.NewClient(gh.URL, tokens)
	if _, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: "gh:" + githubtest.Repo + "#3", Head: "istari/3/x", Base: "main", Title: "X"}); err != nil {
		t.Fatalf("an App call: %v", err)
	}
}

func TestAnOpenedPullRequestLinksToItsTicket(t *testing.T) {
	gh := githubtest.New(t)
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: "gh:" + githubtest.Repo + "#3", Head: "istari/3/add-limits", Base: "main", Title: "Add limits", Body: "## Summary\n\nLimits."})
	if err != nil {
		t.Fatal(err)
	}
	got := gh.Issue(pr.Number)
	if !strings.Contains(got.Body, "Closes #3") || !strings.Contains(got.Body, "## Summary") || got.Branch != "istari/3/add-limits" || pr.URL == "" {
		t.Fatalf("pull request = %+v (%+v)", got, pr)
	}
}

func TestCheckAndReviewStateSurfaceAsEvents(t *testing.T) {
	gh := githubtest.New(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	w := vcs.NewWatcher(c, s, nil)
	ticket := "gh:" + githubtest.Repo + "#3"
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: ticket, Head: "istari/3/x", Base: "main", Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Track(ctx, ticket, githubtest.Repo, pr); err != nil {
		t.Fatal(err)
	}
	gh.SetHeadSHA(pr.Number, "abc123")
	gh.SetChecks("abc123", githubtest.Check{Name: "test", Status: "in_progress"})
	poll := func() {
		t.Helper()
		if err := w.Poll(ctx); err != nil {
			t.Fatalf("Poll: %v", err)
		}
	}
	poll()
	gh.SetChecks("abc123", githubtest.Check{Name: "test", Status: "completed", Conclusion: "success"})
	gh.AddReview(pr.Number, "alex", "APPROVED")
	poll()
	poll() // nothing new
	type change struct{ Type, Checks, Review string }
	var got []change
	events, _ := s.Read(ctx, "", store.Filter{Ticket: ticket, TypePrefix: "vcs."}, 0)
	for _, e := range events {
		if e.Type == "vcs.tracked" {
			continue
		}
		var p struct{ Checks, Review string }
		json.Unmarshal(e.Payload, &p)
		got = append(got, change{e.Type, p.Checks, p.Review})
	}
	want := []change{{"vcs.checks", "pending", ""}, {"vcs.checks", "success", ""}, {"vcs.review", "", "approved"}}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %+v, want %+v", got, want)
		}
	}
}

func TestMergingUsesTheMethodAndDeletesTheBranch(t *testing.T) {
	gh := githubtest.New(t)
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: "gh:" + githubtest.Repo + "#3", Head: "istari/3/x", Base: "main", Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Merge(ctx, githubtest.Repo, pr.Number, "squash"); err != nil {
		t.Fatal(err)
	}
	if got := gh.Issue(pr.Number); !got.Merged || got.MergeMethod != "squash" || len(gh.DeletedBranches()) != 1 || gh.DeletedBranches()[0] != "istari/3/x" {
		t.Fatalf("after merge: %+v, deleted %v", got, gh.DeletedBranches())
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pushFixture is a remote with main, a clone of istari's (the agent's
// worktree shares its config) on a ticket branch, and istari's private push
// repository.
func pushFixture(t *testing.T) (root, remote, work string, spec vcs.PushSpec) {
	t.Helper()
	root = t.TempDir()
	remote = filepath.Join(root, "web.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", remote)
	work = filepath.Join(root, "work")
	git(t, root, "clone", "-q", remote, work)
	git(t, work, "commit", "-q", "--allow-empty", "-m", "initial")
	git(t, work, "push", "-q", "origin", "main")
	git(t, work, "checkout", "-q", "-b", "istari/7/x")
	git(t, work, "commit", "-q", "--allow-empty", "-m", "work")
	return root, remote, work, vcs.PushSpec{Worktree: work, URL: remote, Private: filepath.Join(root, "push", "web.git"), Own: "istari/7/x", Target: "istari/7/x"}
}

func TestAPushGoesOnlyToTheOwnBranchAndMayForceWithLease(t *testing.T) {
	root, remote, work, spec := pushFixture(t)
	own := spec.Own
	if err := vcs.Push(ctx, spec); err != nil {
		t.Fatalf("push to the own branch: %v", err)
	}
	for _, target := range []string{"main", "istari/8/other", "istari/7/x:refs/heads/main"} {
		other := spec
		other.Target = target
		if err := vcs.Push(ctx, other); err == nil {
			t.Fatalf("a push to %s was allowed", target)
		}
	}
	git(t, work, "commit", "-q", "--amend", "--allow-empty", "-m", "work, rebased")
	if err := vcs.Push(ctx, spec); err == nil {
		t.Fatal("a rewritten history was pushed without force")
	}
	forced := spec
	forced.Force = true
	if err := vcs.Push(ctx, forced); err != nil {
		t.Fatalf("force with lease to the own branch: %v", err)
	}
	if head, remoteHead := git(t, work, "rev-parse", "HEAD"), git(t, remote, "rev-parse", own); head != remoteHead {
		t.Fatalf("remote %s, want %s", remoteHead, head)
	}
	// Someone else pushes to the branch; the agent moves its own view of it.
	other := filepath.Join(root, "other")
	git(t, root, "clone", "-q", "-b", own, remote, other)
	git(t, other, "commit", "-q", "--allow-empty", "-m", "theirs")
	git(t, other, "push", "-q", "origin", own)
	theirs := git(t, other, "rev-parse", "HEAD")
	git(t, work, "fetch", "-q", "origin")
	git(t, work, "commit", "-q", "--amend", "--allow-empty", "-m", "mine again")
	if err := vcs.Push(ctx, forced); err == nil {
		t.Fatal("a forced push overwrote work istari had not seen")
	}
	if got := git(t, remote, "rev-parse", own); got != theirs {
		t.Fatalf("remote %s, want their %s kept", got, theirs)
	}
}

func TestAnAgentCannotRedirectThePush(t *testing.T) {
	root, remote, work, spec := pushFixture(t)
	elsewhere := filepath.Join(root, "elsewhere.git")
	git(t, root, "init", "-q", "--bare", elsewhere)
	// What an agent could do in its worktree, whose config it shares.
	git(t, work, "config", "remote.origin.pushurl", elsewhere)
	git(t, work, "config", "url."+elsewhere+".pushInsteadOf", remote)
	hooks := filepath.Join(root, "evil-hooks")
	os.MkdirAll(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nenv > "+filepath.Join(root, "stolen")+"\n"), 0o755)
	git(t, work, "config", "core.hooksPath", hooks)
	spec.Token = "secret-installation-token"
	if err := vcs.Push(ctx, spec); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "stolen")); err == nil {
		t.Fatal("the agent's hook ran during istari's push")
	}
	if out, err := exec.Command("git", "-C", elsewhere, "rev-parse", "--verify", spec.Own).CombinedOutput(); err == nil {
		t.Fatalf("the push went elsewhere: %s", out)
	}
	if got := git(t, remote, "rev-parse", spec.Own); got != git(t, work, "rev-parse", "HEAD") {
		t.Fatal("the push did not reach the repository's own URL")
	}
}

func TestAMergedPullRequestIsNoLongerPolled(t *testing.T) {
	gh := githubtest.New(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	w := vcs.NewWatcher(c, s, nil)
	ticket := "gh:" + githubtest.Repo + "#3"
	pr, _ := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: ticket, Head: "istari/3/x", Base: "main", Title: "X"})
	w.Track(ctx, ticket, githubtest.Repo, pr)
	if err := c.Merge(ctx, githubtest.Repo, pr.Number, "squash"); err != nil {
		t.Fatal(err)
	}
	w.Poll(ctx)
	if tr, err := w.PullRequestOf(ctx, ticket); err != nil || !tr.Done {
		t.Fatalf("after the merge: %+v, %v; want done", tr, err)
	}
}

func TestADismissedReviewAndCommitStatusesAreSeen(t *testing.T) {
	gh := githubtest.New(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	w := vcs.NewWatcher(c, s, nil)
	ticket := "gh:" + githubtest.Repo + "#3"
	pr, _ := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: ticket, Head: "istari/3/x", Base: "main", Title: "X"})
	w.Track(ctx, ticket, githubtest.Repo, pr)
	gh.SetHeadSHA(pr.Number, "def456")
	gh.SetStatus("def456", "failure") // a repository with commit statuses only
	gh.AddReview(pr.Number, "alex", "CHANGES_REQUESTED")
	w.Poll(ctx)
	gh.DismissReviews(pr.Number)
	w.Poll(ctx)
	var got []string
	events, _ := s.Read(ctx, "", store.Filter{Ticket: ticket, TypePrefix: "vcs."}, 0)
	for _, e := range events {
		var p struct{ Checks, Review string }
		json.Unmarshal(e.Payload, &p)
		got = append(got, e.Type+":"+p.Checks+p.Review)
	}
	if want := "vcs.tracked: vcs.checks:failure vcs.review:changes_requested vcs.review:none"; strings.Join(got, " ") != want {
		t.Fatalf("events = %s, want %s", strings.Join(got, " "), want)
	}
}

func TestAPullRequestCanBeUpdatedAndKeepsItsTicketLink(t *testing.T) {
	gh := githubtest.New(t)
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	ticket := "gh:" + githubtest.Repo + "#3"
	pr, _ := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: ticket, Head: "istari/3/x", Base: "main", Title: "X", Body: "old"})
	if err := c.UpdatePullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: ticket, Title: "Y", Body: "new"}, pr.Number); err != nil {
		t.Fatal(err)
	}
	got := gh.Issue(pr.Number)
	if got.Title != "Y" || !strings.Contains(got.Body, "new") || !strings.Contains(got.Body, "Closes #3") || !strings.Contains(got.Body, "Ticket: "+ticket) {
		t.Fatalf("after update: %q %q", got.Title, got.Body)
	}
}

func TestChecksOfACommitOlderThanTheLastPushAreNotTaken(t *testing.T) {
	gh := githubtest.New(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	w := vcs.NewWatcher(c, s, nil)
	ticket := "gh:" + githubtest.Repo + "#3"
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: ticket, Head: "istari/3/x", Base: "main", Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Track(ctx, ticket, githubtest.Repo, pr); err != nil {
		t.Fatal(err)
	}
	gh.SetHeadSHA(pr.Number, "old111")
	gh.SetChecks("old111", githubtest.Check{Name: "test", Status: "completed", Conclusion: "success"})
	if err := w.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Pushed(ctx, ticket, "new222"); err != nil { // the rework round pushed
		t.Fatal(err)
	}
	if err := w.Poll(ctx); err != nil { // GitHub still shows the old head
		t.Fatal(err)
	}
	if tr, _ := w.PullRequestOf(ctx, ticket); tr.Checks != "" {
		t.Fatalf("after the push the old commit's checks are taken: %+v", tr)
	}
	gh.SetHeadSHA(pr.Number, "new222")
	gh.SetChecks("new222", githubtest.Check{Name: "test", Status: "completed", Conclusion: "failure"})
	if err := w.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if tr, _ := w.PullRequestOf(ctx, ticket); tr.Checks != "failure" || tr.SHA != "new222" {
		t.Fatalf("the pushed commit's checks: %+v", tr)
	}
}

func TestAPullRequestIsFoundByItsURL(t *testing.T) {
	if pr, ok := vcs.PullRequestAt("https://github.com/acme/web/pull/42"); !ok || pr.Number != 42 {
		t.Fatalf("PullRequestAt = %+v, %v", pr, ok)
	}
	for _, url := range []string{"", "https://github.com/acme/web/issues/42", "https://github.com/acme/web/pull/x", "https://github.com/acme/web/pull/0"} {
		if _, ok := vcs.PullRequestAt(url); ok {
			t.Errorf("%q is taken for a pull request", url)
		}
	}
}

func TestADraftPullRequestIsReadyForReviewAfterMarkReady(t *testing.T) {
	gh := githubtest.New(t)
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: "gh:" + githubtest.Repo + "#3", Head: "istari/3/x", Base: "main", Title: "X", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if !gh.Issue(pr.Number).Draft {
		t.Fatal("the pull request was not opened as a draft")
	}
	if err := c.MarkReady(ctx, githubtest.Repo, pr.Number); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if gh.Issue(pr.Number).Draft {
		t.Fatal("the pull request is still a draft")
	}
	if err := c.MarkReady(ctx, githubtest.Repo, pr.Number); err != nil {
		t.Fatalf("MarkReady on a ready pull request: %v", err)
	}
	if err := c.MarkReady(ctx, githubtest.Repo, 999); err == nil {
		t.Fatal("MarkReady on a missing pull request succeeded")
	}
}

func TestMarkReadyWorksOnGitHubEnterprise(t *testing.T) {
	gh := githubtest.New(t, githubtest.Prefix("/api/v3"))
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: "gh:" + githubtest.Repo + "#3", Head: "istari/3/x", Base: "main", Title: "X", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MarkReady(ctx, githubtest.Repo, pr.Number); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if gh.Issue(pr.Number).Draft {
		t.Fatal("the pull request is still a draft")
	}
}

func TestAReviewIsPublishedThroughAShortOutageAndUnavailableAfterALongOne(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token)).WithBackoff([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond})
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: "gh:" + githubtest.Repo + "#3", Head: "istari/3/x", Base: "main", Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	review := vcs.Review{Standards: vcs.Axis{Summary: "0 findings"}, Spec: vcs.Axis{Summary: "0 findings"}}
	gh.FailNext(2)
	if err := c.PublishReview(ctx, githubtest.Repo, pr.Number, review); err != nil {
		t.Fatalf("PublishReview after two 500s: %v", err)
	}
	gh.FailNext(4)
	if err := c.PublishReview(ctx, githubtest.Repo, pr.Number, review); !errors.Is(err, tickets.ErrUnavailable) {
		t.Fatalf("PublishReview during an outage: %v, want ErrUnavailable", err)
	}
	gh.FailNextWith(1, http.StatusUnprocessableEntity)
	if err := c.PublishReview(ctx, githubtest.Repo, pr.Number, review); err == nil || errors.Is(err, tickets.ErrUnavailable) {
		t.Fatalf("PublishReview answered 422: %v, want a plain error", err)
	}
	if got := len(gh.Issue(pr.Number).Reviews); got != 1 {
		t.Fatalf("%d reviews, want the one published through the short outage", got)
	}
}

func TestAReviewIsPublishedWithTheTwoAxesApartAndUnranked(t *testing.T) {
	gh := githubtest.New(t)
	c := vcs.NewClient(gh.URL, vcs.StaticToken(gh.Token))
	pr, err := c.OpenPullRequest(ctx, vcs.NewPullRequest{Repo: githubtest.Repo, Ticket: "gh:" + githubtest.Repo + "#3", Head: "istari/3/x", Base: "main", Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.PublishReview(ctx, githubtest.Repo, pr.Number, vcs.Review{
		Standards: vcs.Axis{Summary: "1 finding: Get has no doc comment", Findings: []string{"cache.go:3: Get has no doc comment"}},
		Spec:      vcs.Axis{Summary: "0 findings"},
	})
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	reviews := gh.Issue(pr.Number).Reviews
	if len(reviews) != 1 || reviews[0].State != "COMMENTED" {
		t.Fatalf("reviews = %+v, want one comment review: the verdict is istari's, not GitHub's", reviews)
	}
	body := reviews[0].Body
	standards, spec := strings.Index(body, "## Standards"), strings.Index(body, "## Spec")
	if standards < 0 || spec < standards {
		t.Fatalf("body = %q, want the section Standards, then Spec", body)
	}
	if !strings.Contains(body[standards:spec], "cache.go:3: Get has no doc comment") || !strings.Contains(body[spec:], "0 findings") {
		t.Fatalf("body = %q, want each axis with its own findings", body)
	}
}
