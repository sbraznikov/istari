package runner_test

import (
	"testing"

	"github.com/sbraznikov/istari/internal/runner"
	"github.com/sbraznikov/istari/internal/tickets"
)

func TestATicketBranchIsNamedAfterItsKeyAndTitle(t *testing.T) {
	gh := tickets.Ref{Provider: "gh", Repo: "acme/web", Number: 123}
	for _, tc := range []struct {
		ref   tickets.Ref
		title string
		want  string
	}{
		{gh, "Add rate limits", "istari/123/add-rate-limits"},
		{gh, "  `ticket.fire` must not bypass admission!  ", "istari/123/ticket-fire-must-not-bypass-admission"},
		{gh, "Über-Größe: 2× faster", "istari/123/ber-gr-e-2-faster"},
		{gh, "???", "istari/123/ticket"},
		{tickets.Ref{Provider: "lin", Key: "ENG-42"}, "Fix login", "istari/ENG-42/fix-login"},
		{gh, "A very long title that goes on and on well past any sensible branch length", "istari/123/a-very-long-title-that-goes-on-and-on-well"},
	} {
		if got := runner.Branch(tc.ref, tc.title); got != tc.want {
			t.Errorf("Branch(%s, %q) = %q, want %q", tc.ref, tc.title, got, tc.want)
		}
	}
}
