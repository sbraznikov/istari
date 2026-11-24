package vcs_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
	"github.com/sbraznikov/istari/internal/vcs"
)

func TestTheTicketLinkCheckPassesOnlyForOpenTickets(t *testing.T) {
	gh := githubtest.New(t)
	open := gh.Seed("open", "", "bob", "istari:in_progress")
	closed := gh.Seed("closed", "", "bob", "istari:done")
	gh.HumanClose(closed, "bob", "completed")
	canceled := gh.Seed("canceled", "", "bob", "istari:canceled")
	tracker := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	ref := func(n int) string { return "gh:" + githubtest.Repo + "#" + strconv.Itoa(n) }

	for _, tc := range []struct {
		name     string
		branch   string
		messages []string
		says     string // "" means the check passes
	}{
		{"branch", "istari/" + strconv.Itoa(open) + "/x", []string{"Do x"}, ""},
		{"trailer", "feature/x", []string{"Do x\n\nTicket: " + ref(open)}, ""},
		{"nothing", "feature/x", []string{"Do x"}, "references no ticket"},
		{"closed", "istari/" + strconv.Itoa(closed) + "/x", []string{"Do x"}, "closed"},
		{"canceled", "feature/x", []string{"Do x\n\nTicket: " + ref(canceled)}, "canceled"},
		{"one commit to a closed ticket", "istari/" + strconv.Itoa(open) + "/x", []string{"Do x", "Do y\n\nTicket: " + ref(closed)}, "closed"},
		{"missing", "istari/999/x", nil, "does not exist"},
	} {
		pr := vcs.PullRequest{Repo: githubtest.Repo, Branch: tc.branch, Messages: tc.messages}
		err := vcs.CheckTicketLink(context.Background(), pr, tracker)
		switch {
		case tc.says == "" && err != nil:
			t.Errorf("%s: %v, want a pass", tc.name, err)
		case tc.says != "" && (err == nil || !strings.Contains(err.Error(), tc.says)):
			t.Errorf("%s: %v, want a failure that says %q", tc.name, err, tc.says)
		}
	}
}
