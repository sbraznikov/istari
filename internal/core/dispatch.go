package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sbraznikov/istari/internal/store"
)

// Limits bound how many tasks may be claimed at once (spec section 9 and
// [daemon] max_concurrent_runs). Zero means no limit.
type Limits struct {
	Total      int            // per installation
	PerProject map[string]int // per project name
	PerRole    map[string]int // per role
}

// Event types of the hand-out of tasks.
const (
	EventDispatchPaused  = "dispatch.paused"
	EventDispatchResumed = "dispatch.resumed"
)

const (
	dispatchKind = "dispatch"
	dispatchID   = "state"
)

// dispatchState is the one record every claim and every pause rewrites.
// Because a claim is conditional on its version, two claims cannot both take
// the last free slot, and no claim slips past a pause.
type dispatchState struct {
	PausedAll      bool     `json:"paused_all"`
	PausedProjects []string `json:"paused_projects,omitempty"`
	Claims         int64    `json:"claims"` // claims ever made; bumps the version

	version int64
}

func (t *Tasks) dispatchState(ctx context.Context) (dispatchState, error) {
	rec, err := t.store.Get(ctx, dispatchKind, dispatchID)
	if errors.Is(err, store.ErrNotFound) {
		return dispatchState{}, nil
	}
	if err != nil {
		return dispatchState{}, err
	}
	var d dispatchState
	if err := json.Unmarshal(rec.Data, &d); err != nil {
		return dispatchState{}, fmt.Errorf("core: dispatch state: %w", err)
	}
	d.version = rec.Version
	return d, nil
}

func (d dispatchState) change() store.Change {
	data, err := json.Marshal(d)
	if err != nil {
		panic(fmt.Sprintf("core: marshal dispatch state: %v", err))
	}
	return store.Change{Kind: dispatchKind, ID: dispatchID, Data: data, IfVersion: d.version}
}

// agentActor is the prefix of the actor an agent's run claims with
// (agent:<profile>). Only such a claim starts a run in the ticket's worktree.
const agentActor = "agent:"

// admit checks that actor may claim task now: hand-out is not paused for it,
// no limit is full, and, for an agent, no other run works in the ticket's
// worktree (spec section 7.2). It returns the change that records the claim;
// because every claim rewrites the dispatch record on its version, two claims
// at the same moment cannot both pass these checks.
func (t *Tasks) admit(ctx context.Context, actor string, task Task) (store.Change, error) {
	d, err := t.dispatchState(ctx)
	if err != nil {
		return store.Change{}, err
	}
	if d.PausedAll || slices.Contains(d.PausedProjects, task.Project) {
		return store.Change{}, errorf(CodePaused, "the hand-out of tasks is paused")
	}
	all, err := t.List(ctx)
	if err != nil {
		return store.Change{}, err
	}
	agent := strings.HasPrefix(actor, agentActor)
	var total, project, role int
	for _, other := range all {
		if other.State != TaskClaimed || other.ID == task.ID {
			continue
		}
		if agent && other.Ticket == task.Ticket && strings.HasPrefix(other.Owner, agentActor) {
			return store.Change{}, errorf(CodeTicketBusy, "ticket %s has a run of task %s in its worktree; task %s stays ready and is handed out after that run ends", task.Ticket, other.ID, task.ID)
		}
		total++
		if other.Project == task.Project {
			project++
		}
		if other.Role == task.Role {
			role++
		}
	}
	lim := t.opts.Limits
	switch {
	case lim.Total > 0 && total >= lim.Total:
		return store.Change{}, errorf(CodeLimitReached, "%d of %d tasks are claimed", total, lim.Total)
	case lim.PerProject[task.Project] > 0 && project >= lim.PerProject[task.Project]:
		return store.Change{}, errorf(CodeLimitReached, "%d of %d tasks of project %s are claimed", project, lim.PerProject[task.Project], task.Project)
	case lim.PerRole[task.Role] > 0 && role >= lim.PerRole[task.Role]:
		return store.Change{}, errorf(CodeLimitReached, "%d of %d %s tasks are claimed", role, lim.PerRole[task.Role], task.Role)
	}
	d.Claims++
	return d.change(), nil
}

// Pause stops handing out tasks: of every project when project is empty, or
// of one project. Claimed tasks keep running; only new claims are refused.
func (t *Tasks) Pause(ctx context.Context, actor, project string) error {
	return t.setPause(ctx, actor, project, true)
}

// Resume restarts the hand-out that Pause stopped.
func (t *Tasks) Resume(ctx context.Context, actor, project string) error {
	return t.setPause(ctx, actor, project, false)
}

func (t *Tasks) setPause(ctx context.Context, actor, project string, paused bool) error {
	return retry(func() error {
		d, err := t.dispatchState(ctx)
		if err != nil {
			return err
		}
		scope := "all projects"
		var was bool
		if project == "" {
			was, d.PausedAll = d.PausedAll, paused
		} else {
			scope = "project " + project
			was = slices.Contains(d.PausedProjects, project)
			d.PausedProjects = slices.DeleteFunc(d.PausedProjects, func(p string) bool { return p == project })
			if paused {
				d.PausedProjects = append(d.PausedProjects, project)
			}
		}
		if was == paused {
			return errorf(CodeInvalidState, "the hand-out for %s is already %s", scope, map[bool]string{true: "paused", false: "running"}[paused])
		}
		typ := EventDispatchResumed
		if paused {
			typ = EventDispatchPaused
		}
		payload, _ := json.Marshal(map[string]any{"project": project, "all": project == ""})
		_, err = t.store.Append(ctx, store.Event{Type: typ, At: t.opts.Clock(), Actor: actor, Project: project, Payload: payload}, d.change())
		return err
	})
}
