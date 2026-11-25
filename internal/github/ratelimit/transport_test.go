package ratelimit_test

import (
	"context"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sbraznikov/istari/internal/github/ratelimit"
)

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex // serializes now
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var start = time.Date(2026, 11, 25, 19, 0, 0, 0, time.UTC)

// get sends one GET under kind through the client and returns the body.
func get(t *testing.T, c *http.Client, ctx context.Context, url string, kind ratelimit.Kind, header ...string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ratelimit.WithKind(ctx, kind), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(data)
}

func TestRequestsAreCountedByKindWithTheQuotaOfTheLastAnswer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Reset", "1795638000") // 2026-11-25 20:20:00 UTC
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	clk := &clock{now: start}
	tr := ratelimit.New(ratelimit.Options{Clock: clk.Now})
	c := &http.Client{Transport: tr}
	ctx := context.Background()
	for range 3 {
		get(t, c, ctx, srv.URL+"/repos/o/r/issues", ratelimit.KindSync)
	}
	get(t, c, ctx, srv.URL+"/repos/o/r/pulls/1", ratelimit.KindPRWatch)
	get(t, c, ctx, srv.URL+"/user", "")

	s := tr.Stats()
	want := map[ratelimit.Kind]int{ratelimit.KindSync: 3, ratelimit.KindPRWatch: 1, ratelimit.KindOther: 1}
	if !maps.Equal(s.Requests, want) {
		t.Errorf("requests = %v, want %v", s.Requests, want)
	}
	if s.Limit != 5000 || s.Remaining != 4321 {
		t.Errorf("limit, remaining = %d, %d, want 5000, 4321", s.Limit, s.Remaining)
	}
	if wantReset := time.Date(2026, 11, 25, 20, 20, 0, 0, time.UTC); !s.Reset.Equal(wantReset) {
		t.Errorf("reset = %v, want %v", s.Reset, wantReset)
	}
}

// conditional is a fake GitHub that answers 304 when If-None-Match carries
// the ETag of its one resource, and counts the answers that count against
// the quota (all but 304).
type conditional struct {
	mu      sync.Mutex // serializes the fields below
	counted int
	matched []string // the If-None-Match of each request
}

func (f *conditional) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.matched = append(f.matched, r.Header.Get("If-None-Match"))
	if r.Header.Get("If-None-Match") == `"v1"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f.counted++
	w.Header().Set("ETag", `"v1"`)
	w.Write([]byte(`{"title":"hello"}`))
}

func TestARepeatedReadIsAskedConditionallyAndAnsweredFromTheCache(t *testing.T) {
	t.Parallel()
	fake := &conditional{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	tr := ratelimit.New(ratelimit.Options{})
	c := &http.Client{Transport: tr}
	ctx := context.Background()
	for i := range 2 {
		status, body := get(t, c, ctx, srv.URL+"/repos/o/r/issues/1", ratelimit.KindSync, "Authorization", "Bearer one")
		if status != http.StatusOK || body != `{"title":"hello"}` {
			t.Errorf("read %d = %d %q, want 200 %q", i+1, status, body, `{"title":"hello"}`)
		}
	}
	if fake.counted != 1 {
		t.Errorf("answers that count at GitHub = %d, want 1", fake.counted)
	}
	want := map[ratelimit.Kind]int{ratelimit.KindSync: 1}
	if got := tr.Stats().Requests; !maps.Equal(got, want) {
		t.Errorf("counted requests = %v, want %v", got, want)
	}
}

func TestTheCacheOfOneTokenIsNotUsedForAnother(t *testing.T) {
	t.Parallel()
	fake := &conditional{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c := &http.Client{Transport: ratelimit.New(ratelimit.Options{})}
	ctx := context.Background()
	get(t, c, ctx, srv.URL+"/x", ratelimit.KindSync, "Authorization", "Bearer one")
	get(t, c, ctx, srv.URL+"/x", ratelimit.KindSync, "Authorization", "Bearer two")
	if want := []string{"", ""}; !slices.Equal(fake.matched, want) {
		t.Errorf("If-None-Match sent = %q, want %q", fake.matched, want)
	}
}

// Not parallel: sets the output of the standard logger.
func TestALowQuotaHoldsTheLoopsUntilTheResetAndIsLoggedOnce(t *testing.T) {
	reset := time.Unix(1795638000, 0).UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "50")
		w.Header().Set("X-RateLimit-Reset", "1795638000")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	clk := &clock{now: reset.Add(-30 * time.Millisecond)}
	tr := ratelimit.New(ratelimit.Options{Clock: clk.Now})
	get(t, &http.Client{Transport: tr}, context.Background(), srv.URL+"/x", ratelimit.KindSync)

	for _, loop := range []string{"the ticket sync", "the PR watch"} {
		began := time.Now()
		if err := tr.Hold(context.Background(), loop); err != nil {
			t.Fatalf("Hold(%s): %v", loop, err)
		}
		if waited := time.Since(began); waited < 30*time.Millisecond {
			t.Errorf("Hold(%s) waited %v, want until the reset 30ms away", loop, waited)
		}
	}
	if got := strings.Count(logged.String(), "\n"); got != 1 {
		t.Errorf("log = %q, want one line", logged.String())
	}
	if !strings.Contains(logged.String(), "50") || !strings.Contains(logged.String(), "20:20:00") {
		t.Errorf("log = %q, want the remaining 50 and the reset 20:20:00", logged.String())
	}
}

func TestEnoughQuotaDoesNotHold(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.Header().Set("X-RateLimit-Reset", "1795638000")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a hold would end at once with ctx's error
	tr := ratelimit.New(ratelimit.Options{Clock: (&clock{now: start}).Now})
	get(t, &http.Client{Transport: tr}, context.Background(), srv.URL+"/x", ratelimit.KindSync)
	if err := tr.Hold(ctx, "the ticket sync"); err != nil {
		t.Errorf("Hold with 4000 left = %v, want no hold", err)
	}
}

func TestAnAnswerRefusedForTheRateLimitIsToldFromOtherRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		header []string
		want   bool
	}{
		{"429", http.StatusTooManyRequests, nil, true},
		{"403 with the quota spent", http.StatusForbidden, []string{"X-RateLimit-Remaining", "0"}, true},
		{"403 of the secondary limit", http.StatusForbidden, []string{"Retry-After", "60"}, true},
		{"403 without the right", http.StatusForbidden, []string{"X-RateLimit-Remaining", "4000"}, false},
		{"404", http.StatusNotFound, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for i := 0; i+1 < len(c.header); i += 2 {
					w.Header().Set(c.header[i], c.header[i+1])
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			res, err := http.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if got := ratelimit.RateLimited(res); got != c.want {
				t.Errorf("RateLimited = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRequestsOlderThanAnHourAreNotCounted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	clk := &clock{now: start}
	tr := ratelimit.New(ratelimit.Options{Clock: clk.Now})
	c := &http.Client{Transport: tr}
	ctx := context.Background()
	get(t, c, ctx, srv.URL+"/a", ratelimit.KindSync)
	get(t, c, ctx, srv.URL+"/a", ratelimit.KindSync)
	clk.Advance(30 * time.Minute)
	get(t, c, ctx, srv.URL+"/b", ratelimit.KindFiles)
	clk.Advance(31 * time.Minute)

	want := map[ratelimit.Kind]int{ratelimit.KindFiles: 1}
	if got := tr.Stats().Requests; !maps.Equal(got, want) {
		t.Errorf("requests after 61 minutes = %v, want %v", got, want)
	}
	if got := tr.Stats().Known(); got {
		t.Errorf("known without rate-limit headers = %v, want false", got)
	}
}
