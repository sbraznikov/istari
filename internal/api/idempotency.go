package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/sbraznikov/istari/internal/core"
	"github.com/sbraznikov/istari/internal/store"
)

// IdempotencyHeader carries the idempotency key of a write (spec section 9:
// --idempotency-key on writing commands).
const IdempotencyHeader = "Idempotency-Key"

// requestKind is the projection kind of the record a keyed write leaves.
const requestKind = "request"

// requestRecord is what a keyed write records, in the same transaction as the
// write itself: either both exist or neither does. A repeat of the key does
// not perform the write again; it answers from the current projections.
type requestRecord struct {
	Op     string `json:"op"`
	Actor  string `json:"actor"`
	Task   string `json:"task,omitempty"`
	Ticket string `json:"ticket,omitempty"`
}

type requestKeyCtx struct{}

type requestKey struct {
	key, op string
}

// keyLocks serializes requests that carry the same key.
type keyLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (k *keyLocks) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &sync.Mutex{}
		k.locks[key] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// idempotent runs a write once per key. A repeat with the same key answers
// through again; the same key on another operation is refused.
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
		if done.Op != name {
			return nil, &core.Error{Code: "idempotency_mismatch", Message: fmt.Sprintf("idempotency key %q was used for %s", key, done.Op)}
		}
		return op.again(ctx, done)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	return op.run(context.WithValue(ctx, requestKeyCtx{}, requestKey{key: key, op: name}), raw)
}

// recordRequest returns the change that records a keyed write together with
// its event, or nothing when the request carries no key.
func recordRequest(ctx context.Context, e store.Event) []store.Change {
	k, ok := ctx.Value(requestKeyCtx{}).(requestKey)
	if !ok {
		return nil
	}
	data, _ := json.Marshal(requestRecord{Op: k.op, Actor: e.Actor, Task: e.Task, Ticket: e.Ticket})
	return []store.Change{{Kind: requestKind, ID: k.key, Data: data}}
}
