package runner

import (
	"context"
	"log"
	"time"

	"github.com/sbraznikov/istari/internal/harness"
)

// The pause after a harness hit its account's usage limit without naming
// when it resets (spec section 7.3): DefaultLimitBackoff, doubled on every
// further hit in a row, at most maxLimitBackoff.
const (
	DefaultLimitBackoff = 15 * time.Minute
	maxLimitBackoff     = 2 * time.Hour
)

// EventHarnessLimited records that a harness hit its account's usage limit
// and hands out no task until the pause ends (spec section 8).
const EventHarnessLimited = "harness.limited"

// harnessLimit is the usage-limit pause of one harness. There is one account
// per harness: every slot of the harness pauses with it.
type harnessLimit struct {
	message string
	until   time.Time
	hits    int // hits in a row without a reset time, for the backoff
}

// limit pauses the run's harness until the limit's reset, or for a backoff
// when it named none, and records the pause. A hit while the harness is
// already paused (another slot's run) only extends the pause.
func (r *Runner) limit(ctx context.Context, run Run, e harness.Event) {
	now := r.opts.Clock()
	r.mu.Lock()
	l := r.limits[run.Harness]
	active := l.until.After(now)
	until := e.Reset
	if !until.After(now) {
		if !active {
			l.hits++
		}
		until = now.Add(min(DefaultLimitBackoff<<max(l.hits-1, 0), maxLimitBackoff))
	}
	extended := until.After(l.until)
	if extended {
		l.until, l.message = until, e.Text
	}
	r.limits[run.Harness] = l
	r.mu.Unlock()
	if !extended {
		return
	}
	log.Printf("istari: runner: harness %s paused until %s: the usage limit: %s", run.Harness, until.Local().Format(time.TimeOnly), e.Text)
	r.event(ctx, run, EventHarnessLimited, map[string]any{"harness": run.Harness, "run": run.ID, "message": e.Text, "until": until.UTC()})
}

// unlimit ends the backoff of a harness whose run did not hit the limit.
func (r *Runner) unlimit(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.limits[name]; ok && !l.until.After(r.opts.Clock()) {
		delete(r.limits, name)
	}
}

// limited returns the usage-limit pause of a harness, if it lasts now.
func (r *Runner) limited(name string) (harnessLimit, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.limits[name]
	return l, ok && l.until.After(r.opts.Clock())
}
