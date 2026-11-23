package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

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

// admit checks that task may be claimed now: hand-out is not paused for it and
// no limit is full. It returns the change that records the claim.
func (t *Tasks) admit(ctx context.Context, task Task) (store.Change, error) {
	d, err := t.dispatchState(ctx)
	if err != nil {
		return store.Change{}, err
	}
	if d.PausedAll || slices.Contains(d.PausedProjects, task.Project) {
		return store.Change{}, errorf(CodePaused, "the hand-out of tasks is paused")
	}
	lim := t.opts.Limits
	if lim.Total > 0 || len(lim.PerProject) > 0 || len(lim.PerRole) > 0 {
		all, err := t.List(ctx)
		if err != nil {
			return store.Change{}, err
		}
		var total, project, role int
		for _, other := range all {
			if other.State != TaskClaimed || other.ID == task.ID {
				continue
			}
			total++
			if other.Project == task.Project {
				project++
			}
			if other.Role == task.Role {
				role++
			}
		}
		switch {
		case lim.Total > 0 && total >= lim.Total:
			return store.Change{}, errorf(CodeLimitReached, "%d of %d tasks are claimed", total, lim.Total)
		case lim.PerProject[task.Project] > 0 && project >= lim.PerProject[task.Project]:
			return store.Change{}, errorf(CodeLimitReached, "%d of %d tasks of project %s are claimed", project, lim.PerProject[task.Project], task.Project)
		case lim.PerRole[task.Role] > 0 && role >= lim.PerRole[task.Role]:
			return store.Change{}, errorf(CodeLimitReached, "%d of %d %s tasks are claimed", role, lim.PerRole[task.Role], task.Role)
		}
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
