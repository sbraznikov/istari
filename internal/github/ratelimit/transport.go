// Package ratelimit is the HTTP transport istari's GitHub clients share: it
// counts the requests by kind, reads GitHub's X-RateLimit-* headers and
// answers repeated reads conditionally (#208). It sits under both
// tickets/github and vcs, which must not import each other.
package ratelimit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
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

	mu     sync.Mutex // serializes the fields below
	sent   []sent     // oldest first, within Window
	quota  quota      // of the last answer that carried it
	logged time.Time  // the reset whose wait Hold logged
	cache  cache
}

// The ETag cache is bounded: answers of at most maxCachedBody bytes, at most
// maxCachedBytes of them together; the oldest goes first.
const (
	maxCachedBody  = 1 << 20
	maxCachedBytes = 32 << 20
)

// entry is a cached answer.
type entry struct {
	etag   string
	header http.Header
	body   []byte
}

// response is the cached answer for req, with the quota headers of the 304
// that confirmed it.
func (e entry) response(req *http.Request, fresh http.Header) *http.Response {
	h := e.header.Clone()
	for _, k := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-Used"} {
		if v := fresh.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	return &http.Response{
		Status: "200 OK", StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: h, Body: io.NopCloser(bytes.NewReader(e.body)), ContentLength: int64(len(e.body)), Request: req,
	}
}

// cache is a bounded map of entries in insertion order.
type cache struct {
	entries map[string]entry
	order   []string
	bytes   int // of the bodies held
}

func (c *cache) get(key string) (entry, bool) {
	e, ok := c.entries[key]
	return e, ok
}

func (c *cache) put(key string, e entry) {
	if c.entries == nil {
		c.entries = map[string]entry{}
	}
	if old, ok := c.entries[key]; ok {
		c.bytes -= len(old.body)
	} else {
		c.order = append(c.order, key)
	}
	c.entries[key] = e
	c.bytes += len(e.body)
	for c.bytes > maxCachedBytes {
		c.bytes -= len(c.entries[c.order[0]].body)
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
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

// RoundTrip implements http.RoundTripper. A GET whose earlier answer had an
// ETag is sent with If-None-Match; GitHub's 304 does not count against the
// quota, and the caller gets the earlier answer back as a 200.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	key, cacheable := cacheKey(req)
	var cached entry
	asked := false // sent with the ETag of cached
	if cacheable {
		t.mu.Lock()
		cached, asked = t.cache.get(key)
		t.mu.Unlock()
		if asked {
			req = req.Clone(req.Context())
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if q, ok := quotaOf(res.Header); ok {
		t.quota = q
	}
	if res.StatusCode == http.StatusNotModified {
		if !asked {
			return res, nil // the caller's own conditional request
		}
		res.Body.Close()
		return cached.response(req, res.Header), nil
	}
	t.prune()
	t.sent = append(t.sent, sent{at: t.clock(), kind: kindOf(req.Context())})
	if cacheable && res.StatusCode == http.StatusOK && res.Header.Get("ETag") != "" {
		return t.store(key, res)
	}
	return res, nil
}

// store keeps a 200 answer with an ETag for the next conditional read; t.mu
// is held. An answer too large to keep is passed on whole, uncached.
func (t *Transport) store(key string, res *http.Response) (*http.Response, error) {
	body, err := io.ReadAll(io.LimitReader(res.Body, maxCachedBody+1))
	if err != nil {
		res.Body.Close()
		return nil, err
	}
	if len(body) > maxCachedBody {
		res.Body = readCloser{io.MultiReader(bytes.NewReader(body), res.Body), res.Body}
		return res, nil
	}
	res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(body))
	t.cache.put(key, entry{etag: res.Header.Get("ETag"), header: res.Header.Clone(), body: body})
	return res, nil
}

// readCloser reads from one reader and closes another.
type readCloser struct {
	io.Reader
	io.Closer
}

// cacheKey is the key of a GET's cached answer: the URL, what it accepts
// (raw file or JSON) and a hash of its token, so that one token's answer is
// never handed to another. A request with its own If-None-Match is left alone.
func cacheKey(req *http.Request) (string, bool) {
	if req.Method != http.MethodGet || req.Header.Get("If-None-Match") != "" {
		return "", false
	}
	token := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	return req.URL.String() + "\x00" + req.Header.Get("Accept") + "\x00" + hex.EncodeToString(token[:]), true
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

// Low is the remaining quota below which the background loops wait for the
// reset (#208): what is left stays for the operator and for agents' work.
const Low = 100

// Hold waits while the last answer left fewer than Low requests of the
// quota, until its reset, and returns ctx's error if ctx ends first. loop
// names the waiting loop in the log, which says so once per reset. The wait
// is measured with the Transport's clock.
func (t *Transport) Hold(ctx context.Context, loop string) error {
	t.mu.Lock()
	q, now := t.quota, t.clock()
	first := !q.reset.Equal(t.logged)
	if q.remaining < Low && q.reset.After(now) && first {
		t.logged = q.reset
	}
	t.mu.Unlock()
	if q.remaining >= Low || !q.reset.After(now) {
		return nil
	}
	if first {
		log.Printf("istari: GitHub requests: %d of the hour's quota left; %s and the other background loops wait until %s", q.remaining, loop, q.reset.Format("15:04:05 UTC"))
	}
	timer := time.NewTimer(q.reset.Sub(now))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ErrRateLimited matches an answer of GitHub that refused a request for its
// rate limit: a 429, or a 403 that spent the quota or asks to retry later.
// It means "later", not a fault of the caller (#208).
var ErrRateLimited = errors.New("github: rate limit exceeded")

// RateLimited reports whether the answer is GitHub refusing for its rate
// limit.
func RateLimited(res *http.Response) bool {
	switch res.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != ""
	}
	return false
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
