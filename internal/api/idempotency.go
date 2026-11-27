package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/scheduler"
	"github.com/sbraznikov/istari/internal/store"
)

// IdempotencyHeader carries the idempotency key of a write (spec section 9:
// --idempotency-key on writing commands).
const IdempotencyHeader = "Idempotency-Key"

// timerKeyPrefix is reserved for the scheduler's own keys.
const timerKeyPrefix = scheduler.KeyPrefix

// CodeIdempotencyMismatch refuses a key that was used for another request.
const CodeIdempotencyMismatch = "idempotency_mismatch"

// requestKind is the projection kind of the record a keyed write leaves.
const requestKind = "request"

// requestRecord is what a keyed write records, in the same transaction as the
// write itself: either both exist or neither does. A repeat of the key does
// not perform the write again; it answers from the current projections.
type requestRecord struct {
	Op     string `json:"op"`
	Body   string `json:"body"` // SHA-256 of the request body
	Actor  string `json:"actor"`
	Task   string `json:"task,omitempty"`
	Ticket string `json:"ticket,omitempty"`
	Timer  string `json:"timer,omitempty"`
}

type requestKeyCtx struct{}

type requestKey struct {
	key, op, body string
}

// keyLocks serializes requests that carry the same key; a key's lock lives
// only while requests hold or wait for it.
type keyLocks struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	users int
}

func (k *keyLocks) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyLock{}
		k.locks[key] = l
	}
	l.users++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

func bodyHash(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// idempotent runs a write once per key. A repeat of the same request with the
// same key answers through again; the same key on any other request (another
// operation, actor or argument) is refused, so a repeat never hands out what
// another caller received.
func (s *Server) idempotent(ctx context.Context, key, name string, op operation, raw json.RawMessage) (any, error) {
	unlock := s.keys.lock(key)
	defer unlock()
	rec, err := s.store.Get(ctx, requestKind, key)
	switch {
	case err == nil:
		var done requestRecord
		if err := json.Unmarshal(rec.Data, &done); err != nil {
			return nil, err
		}
		if done.Op != name || done.Body != bodyHash(raw) {
			return nil, &core.Error{Code: CodeIdempotencyMismatch, Message: fmt.Sprintf("idempotency key %q was used for another request (%s)", key, done.Op)}
		}
		return op.again(ctx, done)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	return op.run(context.WithValue(ctx, requestKeyCtx{}, requestKey{key: key, op: name, body: bodyHash(raw)}), raw)
}

// recordRequest returns the change that records a keyed write together with
// its event, or nothing when the request carries no key.
func recordRequest(ctx context.Context, e store.Event) []store.Change {
	k, ok := ctx.Value(requestKeyCtx{}).(requestKey)
	if !ok {
		return nil
	}
	rec := requestRecord{Op: k.op, Body: k.body, Actor: e.Actor, Task: e.Task, Ticket: e.Ticket}
	if e.Type == scheduler.EventTimerScheduled {
		var p struct{ Timer string }
		json.Unmarshal(e.Payload, &p) //nolint:errcheck // the payload is the scheduler's own JSON; without a timer name the record keeps none
		rec.Timer = p.Timer
	}
	data, _ := json.Marshal(rec)
	return []store.Change{{Kind: requestKind, ID: k.key, Data: data}}
}
