//go:build sqlite

// Package store persists request/response events into a SQLite database so
// users can inspect historical traffic after the proxy exits.
//
// The implementation uses modernc.org/sqlite, a pure-Go SQLite driver, to
// avoid CGO. The store is best-effort: a failure to write never breaks the
// proxy.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Event is a row persisted to SQLite.
type Event struct {
	Time    time.Time
	Kind    string
	Model   string
	Content string
}

// Store wraps a SQLite database handle.
type Store struct {
	mu sync.Mutex
	db *sql.DB
}

// Open creates (or opens) a SQLite database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			kind TEXT NOT NULL,
			model TEXT NOT NULL,
			content TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
	`)
	if err != nil {
		return fmt.Errorf("sqlite init: %w", err)
	}
	return nil
}

// Append writes a single event. Errors are returned for the caller to log.
func (s *Store) Append(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO events(ts, kind, model, content) VALUES (?, ?, ?, ?)`,
		e.Time.UTC().Format(time.RFC3339Nano), e.Kind, e.Model, e.Content,
	)
	return err
}

// Close flushes and closes the underlying database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// DumpJSON returns all stored events as a JSON array.
func (s *Store) DumpJSON() ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not open")
	}
	rows, err := s.db.Query(`SELECT ts, kind, model, content FROM events ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var ts, k, m, c string
		if err := rows.Scan(&ts, &k, &m, &c); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"time": ts, "kind": k, "model": m, "content": c})
	}
	return json.Marshal(out)
}
