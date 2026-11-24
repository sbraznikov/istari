package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

// ticketLink runs istari ci ticket-link as the GitHub Action does: with
// GITHUB_TOKEN and GITHUB_API_URL and no gh on PATH.
func ticketLink(t *testing.T, gh *githubtest.Server, pr int) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary, "ci", "ticket-link", "--repo", githubtest.Repo, "--pr", strconv.Itoa(pr))
	cmd.Env = append(os.Environ(), "GITHUB_TOKEN="+gh.Token, "GITHUB_API_URL="+gh.URL, "PATH=/nonexistent")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return out.String(), errOut.String(), code
}

func TestCITicketLinkPassesAPullRequestOfAnOpenTicketOnly(t *testing.T) {
	gh := githubtest.New(t)
	open := gh.Seed("open", "", "bob", "istari:in_progress")
	canceled := gh.Seed("canceled", "", "bob", "istari:canceled")
	good := gh.SeedPull(fmt.Sprintf("istari/%d/add-limits", open), "Add limits\n\nTicket: gh:"+githubtest.Repo+"#"+strconv.Itoa(open))
	none := gh.SeedPull("feature/x", "Add limits")
	stale := gh.SeedPull(fmt.Sprintf("istari/%d/x", canceled), "Add limits")

	if out, stderr, code := ticketLink(t, gh, good); code != 0 || !strings.Contains(out, fmt.Sprintf("#%d", open)) {
		t.Fatalf("an open ticket's pull request: exit %d %q %q", code, out, stderr)
	}
	if _, stderr, code := ticketLink(t, gh, none); code != 1 || !strings.Contains(stderr, "references no ticket") {
		t.Fatalf("a pull request without a ticket: exit %d %q", code, stderr)
	}
	if _, stderr, code := ticketLink(t, gh, stale); code != 1 || !strings.Contains(stderr, "canceled") {
		t.Fatalf("a canceled ticket's pull request: exit %d %q", code, stderr)
	}
	// Every commit counts, past the first page.
	gh.SetPageSize(1)
	late := gh.SeedPull(fmt.Sprintf("istari/%d/x", open), "Add limits", "Fix\n\nTicket: gh:"+githubtest.Repo+"#"+strconv.Itoa(canceled))
	if _, stderr, code := ticketLink(t, gh, late); code != 1 || !strings.Contains(stderr, "canceled") {
		t.Fatalf("a canceled ticket in the second commit: exit %d %q", code, stderr)
	}
}
