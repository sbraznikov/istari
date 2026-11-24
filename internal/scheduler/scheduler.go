// Package scheduler drives time in the daemon (spec section 13: leases,
// timers, limits): on every tick it expires leases whose deadline has passed
// and fires the delayed actions that are due.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
)

// KeyPrefix starts the idempotency key of every timer's action; clients may
// not use keys of this form.
const KeyPrefix = "timer:"

// CodeInvalidTimer refuses a delayed action that could never run.
const CodeInvalidTimer = "invalid_timer"

// DefaultTick is how often the scheduler looks at the clock.
const DefaultTick = time.Second

// Exec performs one API write operation with an idempotency key. A refusal is
// returned as a *core.Error; any other error means the action did not run and
// is tried again on the next tick.
type Exec func(ctx context.Context, op, key string, request json.RawMessage) error

// Options configures a Scheduler.
type Options struct {
	Clock func() time.Time
	Tick  time.Duration
	// Prepare checks that op is a write operation and request a valid request
	// for it, now rather than when the timer fires, and may complete the
	// request, e.g. with the actor of the one who schedules.
	Prepare func(op string, request json.RawMessage, actor string) (json.RawMessage, error)
}

// transient are refusals that may pass later: the action is tried again on
// the next tick instead of being recorded as refused.
var transient = []string{core.CodePaused, core.CodeLimitReached, core.CodeNoReadyTask}

// TimerState is the state of a delayed action.
type TimerState string

// Timer states.
const (
	TimerPending TimerState = "pending"
	TimerFired   TimerState = "fired"
)

// Timer is one delayed action: an API operation and its request, due at At.
type Timer struct {
	ID      string          `json:"id"`
	At      time.Time       `json:"at"`
	Op      string          `json:"op"`
	Request json.RawMessage `json:"request"`
	Actor   string          `json:"actor"`
	State   TimerState      `json:"state"`
	Result  string          `json:"result,omitempty"` // the refusal, if the action was refused

	version int64
}

// Event types of timers.
const (
	EventTimerScheduled = "timer.scheduled"
	EventTimerFired     = "timer.fired"
)

const timerKind = "timer"

// Scheduler expires leases and fires timers.
type Scheduler struct {
	store store.Store
	tasks *core.Tasks
	exec  Exec
	opts  Options
}

// New returns a scheduler. It writes through s, the same store the daemon's
// API writes through.
func New(s store.Store, tasks *core.Tasks, exec Exec, opts Options) *Scheduler {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	return &Scheduler{store: s, tasks: tasks, exec: exec, opts: opts}
}

// Schedule records a delayed action. It fires once, at or after at, and
// survives daemon restarts.
func (s *Scheduler) Schedule(ctx context.Context, actor string, at time.Time, op string, request json.RawMessage) (Timer, error) {
	if actor == "" {
		return Timer{}, &core.Error{Code: CodeInvalidTimer, Message: "a scheduled action needs an actor"}
	}
	if len(request) == 0 {
		request = json.RawMessage(`{}`)
	}
	if !json.Valid(request) {
		return Timer{}, &core.Error{Code: CodeInvalidTimer, Message: "the request of a scheduled action is not JSON"}
	}
	if s.opts.Prepare != nil {
		var err error
		if request, err = s.opts.Prepare(op, request, actor); err != nil {
			return Timer{}, &core.Error{Code: CodeInvalidTimer, Message: err.Error(), Err: err}
		}
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return Timer{}, err
	}
	timer := Timer{ID: "tm_" + hex.EncodeToString(b), At: at.UTC(), Op: op, Request: request, Actor: actor, State: TimerPending}
	if err := s.commit(ctx, actor, EventTimerScheduled, timer); err != nil {
		return Timer{}, err
	}
	return timer, nil
}

// List returns all timers ordered by id.
func (s *Scheduler) List(ctx context.Context) ([]Timer, error) {
	recs, err := s.store.List(ctx, timerKind)
	if err != nil {
		return nil, err
	}
	timers := make([]Timer, 0, len(recs))
	for _, rec := range recs {
		var timer Timer
		if err := json.Unmarshal(rec.Data, &timer); err != nil {
			return nil, fmt.Errorf("scheduler: timer %s: %w", rec.ID, err)
		}
		timer.version = rec.Version
		timers = append(timers, timer)
	}
	return timers, nil
}

// Step does one tick of work: expire leases, then fire the due timers. A
// timer that fails does not hold back the others.
func (s *Scheduler) Step(ctx context.Context) error {
	var errs []error
	if _, err := s.tasks.ExpireLeases(ctx); err != nil {
		errs = append(errs, fmt.Errorf("scheduler: expiring leases: %w", err))
	}
	timers, err := s.List(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	now := s.opts.Clock()
	for _, timer := range timers {
		if timer.State != TimerPending || now.Before(timer.At) {
			continue
		}
		if err := s.fire(ctx, timer); err != nil {
			errs = append(errs, fmt.Errorf("scheduler: timer %s: %w", timer.ID, err))
		}
	}
	return errors.Join(errs...)
}

// fire runs a due timer's action with the key timer:<id>, then records that it
// fired. If the daemon dies between the two, the next tick runs the action
// again with the same key, which answers from the first run instead of
// acting twice.
func (s *Scheduler) fire(ctx context.Context, timer Timer) error {
	err := s.exec(ctx, timer.Op, KeyPrefix+timer.ID, timer.Request)
	var refused *core.Error
	switch {
	case err == nil:
	case errors.As(err, &refused) && slices.Contains(transient, refused.Code):
		return nil // e.g. paused: the next tick tries again
	case errors.As(err, &refused):
		timer.Result = refused.Error()
	default:
		log.Printf("istari: timer %s (%s) will be retried: %v", timer.ID, timer.Op, err)
		return nil
	}
	timer.State = TimerFired
	err = s.commit(ctx, "system", EventTimerFired, timer)
	if errors.Is(err, store.ErrConflict) {
		return nil // another scheduler recorded it
	}
	return err
}

func (s *Scheduler) commit(ctx context.Context, actor, typ string, timer Timer) error {
	data, err := json.Marshal(timer)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"timer": timer.ID, "op": timer.Op, "at": timer.At, "result": timer.Result})
	_, err = s.store.Append(ctx, store.Event{Type: typ, At: s.opts.Clock(), Actor: actor, Payload: payload},
		store.Change{Kind: timerKind, ID: timer.ID, Data: data, IfVersion: timer.version})
	return err
}

// Run steps on every tick until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.opts.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Step(ctx); err != nil && ctx.Err() == nil {
				log.Printf("istari: scheduler: %v", err)
			}
		}
	}
}
