package tickets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DefaultBackoff retries a write three times within about seven seconds.
var DefaultBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// TransientStatus tells whether an HTTP answer is the host's passing trouble,
// not a refusal of the request: a server error (5xx), a rate limit (429), or
// any answer with an exhausted rate limit (spec section 5).
func TransientStatus(status int, h http.Header) bool {
	return status >= 500 || RateLimited(status, h)
}

// RateLimited tells whether an HTTP answer reports a rate limit.
func RateLimited(status int, h http.Header) bool {
	return status == http.StatusTooManyRequests || h.Get("X-RateLimit-Remaining") == "0"
}

// RetryWrite sends a write, and sends it again after each wait of backoff
// while transient says its error is passing trouble. What still fails so is
// ErrUnavailable; any other error is returned as it is (spec section 5).
func RetryWrite(ctx context.Context, backoff []time.Duration, transient func(error) bool, send func() error) error {
	err := send()
	for _, wait := range backoff {
		if !transient(err) {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
		err = send()
	}
	if transient(err) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}
