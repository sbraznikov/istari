package ratelimit_test

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
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
