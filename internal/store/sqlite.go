package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver; pure Go, no cgo
)

// migrations are applied in order; PRAGMA user_version records how many ran.
// Append new migrations at the end and never edit an applied one.
var migrations = []string{
	`CREATE TABLE events (
		seq     INTEGER PRIMARY KEY AUTOINCREMENT,
		type    TEXT    NOT NULL,
		at      INTEGER NOT NULL,
		actor   TEXT    NOT NULL,
		project TEXT    NOT NULL,
		ticket  TEXT    NOT NULL,
		task    TEXT    NOT NULL,
		run     TEXT    NOT NULL,
		payload BLOB
	);
	CREATE INDEX events_project ON events (project, seq);
	CREATE INDEX events_ticket  ON events (ticket, seq);`,
	`CREATE TABLE records (
		kind    TEXT    NOT NULL,
		id      TEXT    NOT NULL,
		data    BLOB    NOT NULL,
		version INTEGER NOT NULL,
		PRIMARY KEY (kind, id)
	);`,
}

// SQLite is the Store on a SQLite database in WAL mode.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// Open opens or creates the database at path and applies pending migrations.
func Open(path string) (*SQLite, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(FULL)") // the log is the audit: a commit survives power loss
	q.Set("_txlock", "immediate")         // writers take the lock up front instead of failing on upgrade
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	dsn := url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()} // escapes ?, # and % in the path
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.Exec(migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations))); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return tx.Commit()
}

// Close closes the database.
func (s *SQLite) Close() error { return s.db.Close() }

// Append implements Store.
func (s *SQLite) Append(ctx context.Context, e Event, changes ...Change) (Event, error) {
	if e.Type == "" {
		return Event{}, fmt.Errorf("store: append: event has no type")
	}
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		return Event{}, fmt.Errorf("store: append %s: payload is not valid JSON", e.Type)
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.At = e.At.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, fmt.Errorf("store: append: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events (type, at, actor, project, ticket, task, run, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Type, e.At.UnixNano(), e.Actor, e.Project, e.Ticket, e.Task, e.Run, []byte(e.Payload))
	if err != nil {
		return Event{}, fmt.Errorf("store: append %s: %w", e.Type, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Event{}, fmt.Errorf("store: append %s: %w", e.Type, err)
	}
	for _, c := range changes {
		if err := applyChange(ctx, tx, c); err != nil {
			return Event{}, fmt.Errorf("store: append %s: %w", e.Type, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("store: append %s: %w", e.Type, err)
	}
	e.Cursor = cursorOf(seq)
	return e, nil
}

// Read implements Store.
func (s *SQLite) Read(ctx context.Context, after Cursor, f Filter, limit int) ([]Event, error) {
	from, err := seqOf(after)
	if err != nil {
		return nil, err
	}
	query := strings.Builder{}
	query.WriteString(`SELECT seq, type, at, actor, project, ticket, task, run, payload FROM events WHERE seq > ?`)
	args := []any{from}
	if f.Project != "" {
		query.WriteString(` AND project = ?`)
		args = append(args, f.Project)
	}
	if f.Ticket != "" {
		query.WriteString(` AND ticket = ?`)
		args = append(args, f.Ticket)
	}
	if f.TypePrefix != "" {
		// substr instead of LIKE: '_' and '%' in a prefix are literal.
		query.WriteString(` AND substr(type, 1, length(?)) = ?`)
		args = append(args, f.TypePrefix, f.TypePrefix)
	}
	query.WriteString(` ORDER BY seq`)
	if limit > 0 {
		query.WriteString(` LIMIT ?`)
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: read: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var (
			e       Event
			seq, at int64
			payload []byte
		)
		if err := rows.Scan(&seq, &e.Type, &at, &e.Actor, &e.Project, &e.Ticket, &e.Task, &e.Run, &payload); err != nil {
			return nil, fmt.Errorf("store: read: %w", err)
		}
		e.Cursor = cursorOf(seq)
		e.At = time.Unix(0, at).UTC()
		if len(payload) > 0 {
			e.Payload = payload
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read: %w", err)
	}
	return events, nil
}

// applyChange writes one conditional change inside an Append transaction.
func applyChange(ctx context.Context, tx *sql.Tx, c Change) error {
	if c.Kind == "" || c.ID == "" {
		return fmt.Errorf("change without kind or id")
	}
	if len(c.Data) > 0 && !json.Valid(c.Data) {
		return fmt.Errorf("%s %s: data is not valid JSON", c.Kind, c.ID)
	}
	data := []byte(c.Data)
	if len(data) == 0 {
		data = []byte("null")
	}
	var res sql.Result
	var err error
	if c.IfVersion == 0 {
		res, err = tx.ExecContext(ctx,
			`INSERT INTO records (kind, id, data, version) VALUES (?, ?, ?, 1) ON CONFLICT DO NOTHING`,
			c.Kind, c.ID, data)
	} else {
		res, err = tx.ExecContext(ctx,
			`UPDATE records SET data = ?, version = version + 1 WHERE kind = ? AND id = ? AND version = ?`,
			data, c.Kind, c.ID, c.IfVersion)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", c.Kind, c.ID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("%s %s: %w", c.Kind, c.ID, err)
	} else if n != 1 {
		return fmt.Errorf("%s %s at version %d: %w", c.Kind, c.ID, c.IfVersion, ErrConflict)
	}
	return nil
}

// Get implements Store.
func (s *SQLite) Get(ctx context.Context, kind, id string) (Record, error) {
	r := Record{Kind: kind, ID: id}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data, version FROM records WHERE kind = ? AND id = ?`, kind, id).
		Scan(&data, &r.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("store: get %s %s: %w", kind, id, ErrNotFound)
	}
	if err != nil {
		return Record{}, fmt.Errorf("store: get %s %s: %w", kind, id, err)
	}
	r.Data = data
	return r, nil
}

// List implements Store.
func (s *SQLite) List(ctx context.Context, kind string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, data, version FROM records WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		return nil, fmt.Errorf("store: list %s: %w", kind, err)
	}
	defer rows.Close()
	var recs []Record
	for rows.Next() {
		r := Record{Kind: kind}
		var data []byte
		if err := rows.Scan(&r.ID, &data, &r.Version); err != nil {
			return nil, fmt.Errorf("store: list %s: %w", kind, err)
		}
		r.Data = data
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list %s: %w", kind, err)
	}
	return recs, nil
}

// cursorOf zero-pads the sequence number so that cursors also increase when
// compared as strings.
func cursorOf(seq int64) Cursor { return Cursor(fmt.Sprintf("%020d", seq)) }

func seqOf(c Cursor) (int64, error) {
	if c == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(string(c), 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("store: invalid cursor %q", c)
	}
	return seq, nil
}
