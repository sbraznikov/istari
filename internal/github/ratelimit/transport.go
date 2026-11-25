// Package ratelimit is the HTTP transport istari's GitHub clients share: it
// counts the requests by kind, reads GitHub's X-RateLimit-* headers and
// answers repeated reads conditionally (#208). It sits under both
// tickets/github and vcs, which must not import each other.
package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Kind is what a request is for, as istari doctor reports it.
type Kind string

// The kinds of requests istari makes to GitHub.
const (
	KindSync    Kind = "sync"    // the ticket sync
	KindPRWatch Kind = "prwatch" // the PR watch
	KindJournal Kind = "journal" // journal comments
	KindLabels  Kind = "labels"  // ticket labels
	KindFiles   Kind = "files"   // .istari/ files of main
	KindOther   Kind = "other"   // anything not tagged
)

type kindKey struct{}

// WithKind tags the requests made with ctx as kind.
func WithKind(ctx context.Context, kind Kind) context.Context {
	return context.WithValue(ctx, kindKey{}, kind)
}

// kindOf is the kind ctx was tagged with; untagged is KindOther.
func kindOf(ctx context.Context) Kind {
	if k, ok := ctx.Value(kindKey{}).(Kind); ok && k != "" {
		return k
	}
	return KindOther
}

// Window is how far back Stats counts: GitHub's quota is per hour.
const Window = time.Hour

// Options configures a Transport.
type Options struct {
	Base  http.RoundTripper // nil: http.DefaultTransport
	Clock func() time.Time  // nil: time.Now
}

// Transport is an http.RoundTripper that counts GitHub requests.
type Transport struct {
	base  http.RoundTripper
	clock func() time.Time

	mu    sync.Mutex // serializes the fields below
	sent  []sent     // oldest first, within Window
	quota quota
}

type sent struct {
	at   time.Time
	kind Kind
}

type quota struct {
	limit, remaining int
	reset            time.Time
}

// New returns a Transport.
func New(opts Options) *Transport {
	if opts.Base == nil {
		opts.Base = http.DefaultTransport
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Transport{base: opts.Base, clock: opts.Clock}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append(t.sent, sent{at: t.clock(), kind: kindOf(req.Context())})
	if q, ok := quotaOf(res.Header); ok {
		t.quota = q
	}
	return res, nil
}

// quotaOf reads the X-RateLimit-* headers of an answer.
func quotaOf(h http.Header) (quota, bool) {
	remaining, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return quota{}, false
	}
	q := quota{remaining: remaining}
	q.limit, _ = strconv.Atoi(h.Get("X-RateLimit-Limit")) // 0 when GitHub leaves it out
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		q.reset = time.Unix(reset, 0).UTC()
	}
	return q, true
}

// Stats is what the Transport has seen.
type Stats struct {
	// Requests counts the requests of the last Window that count against
	// the quota, by kind; a kind without any is left out.
	Requests map[Kind]int `json:"requests"`
	// Limit, Remaining and Reset are GitHub's quota from the last answer
	// that carried it; all zero before one did.
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
}

// Known reports whether an answer has told the quota yet.
func (s Stats) Known() bool { return !s.Reset.IsZero() || s.Limit > 0 || s.Remaining > 0 }

// Stats returns the counts of the last Window and the last known quota.
func (t *Transport) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune()
	s := Stats{Requests: map[Kind]int{}, Limit: t.quota.limit, Remaining: t.quota.remaining, Reset: t.quota.reset}
	for _, r := range t.sent {
		s.Requests[r.kind]++
	}
	return s
}

// prune drops the requests older than Window; t.mu is held.
func (t *Transport) prune() {
	cut := t.clock().Add(-Window)
	i := 0
	for i < len(t.sent) && !t.sent[i].at.After(cut) {
		i++
	}
	t.sent = t.sent[i:]
}
