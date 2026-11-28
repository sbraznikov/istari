package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/pipeline"
	"github.com/sbraznikov/istari/internal/store"
	"github.com/sbraznikov/istari/internal/tickets"
	"github.com/sbraznikov/istari/internal/tickets/journal"
	"github.com/sbraznikov/istari/internal/vcs"
)

// EventGateApproved records a merge the pull request's author approved with
// istari gate approve, because GitHub refuses the author's own review (spec
// 4.3); its payload names the approver and the head commit approved.
const EventGateApproved = "gate.approved"

// gateApprovedPayload is the payload of EventGateApproved.
type gateApprovedPayload struct {
	By   string `json:"by"`
	Head string `json:"head"`
}

// gateApproval returns who approved pr at its current head with istari gate
// approve, "" when no approval names that head: a push voids it. GitHub is
// asked for the head only once the ticket has an approval, so a pull request
// that waits for one costs no requests.
func (s *Server) gateApproval(ctx context.Context, pr vcs.Tracked) (string, error) {
	events, err := s.store.Read(ctx, "", store.Filter{Ticket: pr.Ticket, TypePrefix: EventGateApproved}, 0)
	if err != nil || len(events) == 0 {
		return "", err
	}
	_, head, err := s.vcs.Author(ctx, pr.Repo, pr.Number)
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		var p gateApprovedPayload
		if json.Unmarshal(events[i].Payload, &p) == nil && p.Head == head {
			return p.By, nil
		}
	}
	return "", nil
}

// mergeGate is the approve task of a ticket awaiting approval as a gate
// (spec 4.3), with the ticket and its tracked pull request.
type mergeGate struct {
	ticket core.Ticket
	ref    tickets.Ref
	pr     vcs.Tracked
}

// openMergeGate returns task as a merge gate: open when no GitHub App is
// configured, the ticket awaits approval in a merge mode other than manual
// and no gate approval stands at its pull request's head.
func (s *Server) openMergeGate(ctx context.Context, task core.Task) (g mergeGate, open bool, err error) {
	if task.Role != pipeline.RoleGitHubReview || task.State != core.TaskReady || s.githubApp || s.watch == nil {
		return mergeGate{}, false, nil
	}
	if g.ticket, err = s.tickets.Get(ctx, task.Ticket); err != nil || g.ticket.State != core.TicketAwaitingApproval {
		return mergeGate{}, false, err
	}
	if g.ref, err = tickets.ParseRef(task.Ticket); err != nil {
		return mergeGate{}, false, nil
	}
	settings, err := s.engine.SettingsOf(ctx, g.ref.Repo)
	if err != nil || settings.Merge.Mode == pipeline.MergeManual {
		return mergeGate{}, false, err
	}
	g.pr, err = s.watch.PullRequestOf(ctx, task.Ticket)
	if errors.Is(err, store.ErrNotFound) {
		return mergeGate{}, false, nil
	}
	if err != nil {
		return mergeGate{}, false, err
	}
	by, err := s.gateApproval(ctx, g.pr)
	return g, err == nil && by == "", err
}

// approveMerge records actor's approval of the ticket's pull request at its
// current head (spec 4.3): only without a GitHub App, only from a trusted
// member, and only from the pull request's author, whose own review GitHub
// refuses. The next reconcile completes the approve task with it.
func (s *Server) approveMerge(ctx context.Context, actor string, task core.Task) (core.Task, error) {
	if s.githubApp {
		return core.Task{}, &core.Error{Code: core.CodeInvalidTask, Message: fmt.Sprintf("%s is approved by a GitHub review from a member listed in CODEOWNERS: with [github_app] the pull request is the App's, so GitHub takes your review (spec 4.3)", task.ID)}
	}
	g, open, err := s.openMergeGate(ctx, task)
	if err != nil {
		return core.Task{}, err
	}
	if !open {
		return core.Task{}, &core.Error{Code: core.CodeInvalidState, Message: fmt.Sprintf("%s is no open gate: its ticket does not await an approval istari gate approve may give, or the pull request's head is approved already; istari gate list shows the open gates", task.ID)}
	}
	login := strings.TrimPrefix(actor, "human:")
	if s.syncer == nil {
		return core.Task{}, &core.Error{Code: tickets.CodeNotTrusted, Message: "without a ticket sync nobody is trusted to approve a merge with istari gate approve; approve with a GitHub review"}
	}
	if err := s.syncer.RequireTrusted(ctx, g.ref, login); err != nil {
		if e, ok := errors.AsType[*core.Error](err); ok {
			e.Message = "only a trusted member approves a merge with istari gate approve: " + e.Message
		}
		return core.Task{}, err
	}
	author, head, err := s.vcs.Author(ctx, g.pr.Repo, g.pr.Number)
	if err != nil {
		return core.Task{}, err
	}
	if !strings.EqualFold(author, login) {
		return core.Task{}, &core.Error{Code: core.CodeInvalidTask, Message: fmt.Sprintf("%s opened the pull request, not %s: GitHub takes your review, so approve it there; istari gate approve stands in only for the author, whose own review GitHub refuses (spec 4.3)", author, login)}
	}
	payload, err := json.Marshal(gateApprovedPayload{By: login, Head: head})
	if err != nil {
		return core.Task{}, err
	}
	if _, err := s.store.Append(ctx, store.Event{Type: EventGateApproved, At: s.clock().UTC(), Actor: actor, Project: g.ticket.Project, Ticket: task.Ticket, Payload: payload}); err != nil {
		return core.Task{}, err
	}
	note := fmt.Sprintf("%s approved the merge of pull request #%d at %s with istari gate approve: GitHub refuses the author's own review, and no GitHub App is configured (spec 4.3). A push to the branch voids the approval.", actor, g.pr.Number, head)
	if err := s.journal.Append(ctx, g.ref, journal.Entry{Kind: journal.Note, Actor: actor, Text: note}); err != nil {
		return core.Task{}, err
	}
	return task, nil
}
