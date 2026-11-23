// Package store persists istari's state: one ordered, append-only event log
// (spec section 8) and the projection records built from it. Which records an
// event changes is decided by internal/core; store only stores them.
//
// Append is the only write path. It writes one event together with the
// projection changes that event causes, in one transaction, so a projection
// never disagrees with the log.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Cursor identifies a position in the event log. Cursors are opaque and
// increase monotonically; the empty cursor is the position before the first event.
type Cursor string

// Event is one record of the log.
type Event struct {
	Cursor  Cursor          `json:"cursor"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Actor   string          `json:"actor,omitempty"`
	Project string          `json:"project,omitempty"`
	Ticket  string          `json:"ticket,omitempty"`
	Task    string          `json:"task,omitempty"`
	Run     string          `json:"run,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Change sets one projection record as part of an Append.
//
// IfVersion makes the change conditional: 0 means the record must not exist
// yet; any other value must equal the record's current version. A successful
// change stores Data and increments the version.
type Change struct {
	Kind      string
	ID        string
	Data      json.RawMessage
	IfVersion int64
}

// Record is the current value of one projection record.
type Record struct {
	Kind    string
	ID      string
	Data    json.RawMessage
	Version int64
}

// Filter selects events. Empty fields match everything; TypePrefix matches
// event types by prefix, e.g. "task." for the task family.
type Filter struct {
	Project    string
	Ticket     string
	TypePrefix string
}

// Errors returned by a Store.
var (
	// ErrConflict means a Change's IfVersion did not match; nothing was written.
	ErrConflict = errors.New("store: projection version conflict")
	// ErrNotFound means the projection record does not exist.
	ErrNotFound = errors.New("store: record not found")
	// ErrInvalidCursor means a cursor was not issued by this store.
	ErrInvalidCursor = errors.New("store: invalid cursor")
)

// Store is the storage interface. SQLite implements it now; Postgres can later.
type Store interface {
	// Append writes e and changes in one transaction and returns e with its
	// cursor (and its time, when e.At is zero). If any change conflicts,
	// nothing is written and the error wraps ErrConflict.
	Append(ctx context.Context, e Event, changes ...Change) (Event, error)
	// Read returns the events after the cursor that match f, in log order.
	// A limit of 0 means no limit.
	Read(ctx context.Context, after Cursor, f Filter, limit int) ([]Event, error)
	// Get returns one projection record or ErrNotFound.
	Get(ctx context.Context, kind, id string) (Record, error)
	// List returns all records of a kind ordered by id.
	List(ctx context.Context, kind string) ([]Record, error)
	Close() error
}
