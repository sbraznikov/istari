package vcs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/tickets"
)

// PullRequest is what the ticket-link check reads of a pull request.
type PullRequest struct {
	Repo     string   // owner/repo
	Branch   string   // the head branch
	Messages []string // the commit messages
}

// TicketGetter reads tickets from their tracker.
type TicketGetter interface {
	Get(ctx context.Context, ref tickets.Ref) (tickets.Ticket, error)
}

// CheckTicketLink is the required check istari/ticket-link (spec section 2.1,
// level 5): a pull request passes when its branch istari/<number>/<slug> or a
// "Ticket:" trailer of its commits references a ticket, and every ticket it
// references is open and not canceled. The error says why it fails.
func CheckTicketLink(ctx context.Context, pr PullRequest, tracker TicketGetter) error {
	refs := ticketRefs(pr)
	if len(refs) == 0 {
		return fmt.Errorf("the pull request references no ticket: name the branch istari/<ticket>/<slug> or add the trailer \"Ticket: gh:%s#<number>\" to its commits", pr.Repo)
	}
	for _, ref := range refs {
		t, err := tracker.Get(ctx, ref)
		switch {
		case errors.Is(err, tickets.ErrNotFound):
			return fmt.Errorf("ticket %s does not exist", ref)
		case err != nil:
			return fmt.Errorf("ticket %s: %w", ref, err)
		case t.State.State == core.TicketCanceled:
			return fmt.Errorf("ticket %s is canceled", ref)
		case t.Closed:
			return fmt.Errorf("ticket %s is closed", ref)
		}
	}
	return nil
}

// TicketRefsText lists the tickets a pull request references.
func TicketRefsText(pr PullRequest) string {
	var out []string
	for _, r := range ticketRefs(pr) {
		out = append(out, r.String())
	}
	return strings.Join(out, ", ")
}

// ticketRefs returns the tickets a pull request references, each once.
func ticketRefs(pr PullRequest) []tickets.Ref {
	var refs []tickets.Ref
	add := func(r tickets.Ref) {
		for _, have := range refs {
			if have == r {
				return
			}
		}
		refs = append(refs, r)
	}
	if rest, ok := strings.CutPrefix(pr.Branch, "istari/"); ok {
		key, _, _ := strings.Cut(rest, "/")
		if n, err := strconv.Atoi(key); err == nil && n > 0 {
			add(tickets.Ref{Provider: "gh", Repo: pr.Repo, Number: n})
		}
	}
	for _, msg := range pr.Messages {
		for _, line := range strings.Split(msg, "\n") {
			if v, ok := strings.CutPrefix(line, "Ticket:"); ok {
				if ref, err := tickets.ParseRef(strings.TrimSpace(v)); err == nil {
					add(ref)
				}
			}
		}
	}
	return refs
}
