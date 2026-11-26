package tickets_test

import (
	"testing"

	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/github"
	"github.com/sbraznikov/istari/internal/tickets/github/githubtest"
)

func TestDuplicateCandidatesShareThePathsIdentifiersOrKeywordsOfATicket(t *testing.T) {
	t.Parallel()
	gh := githubtest.New(t)
	me := gh.Seed("Rate limits for the export API", "Change `internal/export/limit.go` and `Limiter.Allow`.", "bob")
	samePath := gh.Seed("Faster exports", "Touches `internal/export/limit.go`.", "bob")
	sameIdent := gh.Seed("Burst handling", "Make `Limiter.Allow` handle bursts.", "bob")
	keywords := gh.Seed("Export API rate limits per user", "Users want their own limits.", "bob")
	unrelated := gh.Seed("Fix the login page", "The `web/login.tsx` button is off by one pixel.", "bob")
	canceled := gh.Seed("Rate limits for exports", "Change `internal/export/limit.go`.", "bob")
	gh.HumanClose(canceled, "bob", "not_planned")
	tracker := github.New(github.Options{BaseURL: gh.URL, Token: gh.Token})
	self, err := tracker.Get(ctx, tickets.Ref{Provider: "gh", Repo: githubtest.Repo, Number: me})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tickets.Candidates(ctx, tracker, self)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int]bool{}
	for _, c := range got {
		found[c.Ref.Number] = true
	}
	for _, n := range []int{samePath, sameIdent, keywords} {
		if !found[n] {
			t.Errorf("#%d is not a candidate: %+v", n, got)
		}
	}
	for _, n := range []int{me, unrelated, canceled} {
		if found[n] {
			t.Errorf("#%d is a candidate: %+v", n, got)
		}
	}
	if len(got) > 0 && got[0].Ref.Number != samePath {
		t.Errorf("first candidate = #%d, want the one sharing a path", got[0].Ref.Number)
	}
}
