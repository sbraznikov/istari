package core_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
)

func newLimitedTasks(t *testing.T, limits core.Limits) *core.Tasks {
	t.Helper()
	return limitedTasks(t, limits, 0)
}

// limitedTasks builds a task machine whose clock sleeps for delay, which
// widens the window between checking a limit and committing a claim.
func limitedTasks(t *testing.T, limits core.Limits, delay time.Duration) *core.Tasks {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "istari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	clock := func() time.Time {
		time.Sleep(delay)
		return time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	}
	return core.NewTasks(s, core.TaskOptions{Clock: clock, Limits: limits, Lease: time.Hour})
}

func mustCreate(t *testing.T, tasks *core.Tasks, id, project, role string) {
	t.Helper()
	if _, err := tasks.Create(ctx, "system", core.NewTask{ID: id, Ticket: "gh:acme/" + project + "#1", Project: project, Role: role}); err != nil {
		t.Fatal(err)
	}
}

func TestNoMoreThanMaxConcurrentTasksAreClaimedAtOnce(t *testing.T) {
	t.Parallel()
	const limit, workers = 2, 8
	tasks := limitedTasks(t, core.Limits{Total: limit}, 10*time.Millisecond)
	for i := range workers {
		mustCreate(t, tasks, fmt.Sprintf("t_%d", i), "web", "implementer")
	}
	leases := make(chan core.Lease, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distinct tasks: only the limit stands between the claims.
			lease, err := tasks.Claim(ctx, fmt.Sprintf("agent:%d", i), fmt.Sprintf("t_%d", i))
			if err == nil {
				leases <- lease
				return
			}
			var e *core.Error
			if !errors.As(err, &e) || e.Code != core.CodeLimitReached {
				t.Errorf("claim: %v, want limit_reached", err)
			}
		}()
	}
	wg.Wait()
	close(leases)
	var won []core.Lease
	for l := range leases {
		won = append(won, l)
	}
	if len(won) != limit {
		t.Fatalf("%d claims succeeded with a limit of %d", len(won), limit)
	}
	// A slot frees when a claimed task leaves claimed.
	if err := tasks.Complete(ctx, won[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.ClaimReady(ctx, "agent:next", ""); err != nil {
		t.Fatalf("claim after a slot freed: %v", err)
	}
	_, err := tasks.ClaimReady(ctx, "agent:over", "")
	assertCode(t, err, core.CodeLimitReached)
}

func TestLimitsPerProjectAndPerRole(t *testing.T) {
	t.Parallel()
	tasks := newLimitedTasks(t, core.Limits{PerProject: map[string]int{"web": 1}, PerRole: map[string]int{"reviewer": 1}})
	mustCreate(t, tasks, "web_1", "web", "implementer")
	mustCreate(t, tasks, "web_2", "web", "implementer")
	mustCreate(t, tasks, "api_1", "api", "reviewer")
	mustCreate(t, tasks, "api_2", "api", "reviewer")
	mustCreate(t, tasks, "api_3", "api", "implementer")
	for _, tc := range []struct {
		id   string
		code string
	}{
		{"web_1", ""},
		{"web_2", core.CodeLimitReached}, // project web is full
		{"api_1", ""},
		{"api_2", core.CodeLimitReached}, // role reviewer is full
		{"api_3", ""},                    // another role in another project
	} {
		_, err := tasks.Claim(ctx, "agent:a", tc.id)
		if tc.code == "" && err != nil {
			t.Fatalf("claim %s: %v", tc.id, err)
		}
		if tc.code != "" {
			assertCode(t, err, tc.code)
		}
	}
}

func TestPauseStopsHandOutButNotRunningTasks(t *testing.T) {
	t.Parallel()
	tasks := newLimitedTasks(t, core.Limits{})
	mustCreate(t, tasks, "web_1", "web", "implementer")
	mustCreate(t, tasks, "web_2", "web", "implementer")
	mustCreate(t, tasks, "api_1", "api", "implementer")
	running, err := tasks.Claim(ctx, "agent:a", "web_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.Pause(ctx, "human:alex", ""); err != nil {
		t.Fatalf("Pause all: %v", err)
	}
	_, err = tasks.ClaimReady(ctx, "agent:b", "")
	assertCode(t, err, core.CodePaused)
	if _, err := tasks.Renew(ctx, running); err != nil {
		t.Fatalf("renew while paused: %v", err)
	}
	if err := tasks.Resume(ctx, "human:alex", ""); err != nil {
		t.Fatal(err)
	}
	// Pausing one project leaves the others.
	if err := tasks.Pause(ctx, "human:alex", "web"); err != nil {
		t.Fatal(err)
	}
	_, err = tasks.Claim(ctx, "agent:b", "web_2")
	assertCode(t, err, core.CodePaused)
	if _, err := tasks.Claim(ctx, "agent:b", "api_1"); err != nil {
		t.Fatalf("claim in an unpaused project: %v", err)
	}
	if err := tasks.Resume(ctx, "human:alex", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Claim(ctx, "agent:b", "web_2"); err != nil {
		t.Fatalf("claim after resume: %v", err)
	}
	if err := tasks.Complete(ctx, running); err != nil {
		t.Fatalf("complete a task claimed before the pause: %v", err)
	}
}

func TestClaimReadySkipsTasksThatCannotBeHandedOutAndTakesTheNext(t *testing.T) {
	t.Parallel()
	tasks := newLimitedTasks(t, core.Limits{PerRole: map[string]int{"reviewer": 1}})
	// Created in this order; the clock is fixed, so ties go by id.
	mustCreate(t, tasks, "a_web", "web", "implementer") // first, but its project is paused
	mustCreate(t, tasks, "b_rev", "api", "reviewer")
	mustCreate(t, tasks, "c_rev", "api", "reviewer")
	mustCreate(t, tasks, "d_api", "api", "implementer")
	if err := tasks.Pause(ctx, "human:alex", "web"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for range 2 {
		lease, err := tasks.ClaimReady(ctx, "agent:any", "")
		if err != nil {
			t.Fatalf("ClaimReady: %v", err)
		}
		got = append(got, lease.Task)
	}
	// b_rev fills the reviewer limit, so c_rev is skipped for d_api.
	if got[0] != "b_rev" || got[1] != "d_api" {
		t.Fatalf("claimed %v, want [b_rev d_api]", got)
	}
	_, err := tasks.ClaimReady(ctx, "agent:any", "")
	assertCode(t, err, core.CodePaused) // only kept-back tasks are left: the first one's refusal
}
