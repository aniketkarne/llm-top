//go:build sqlite

// Package store persists request/response events into a SQLite database so
// users can inspect historical traffic after the proxy exits.
//
// The implementation uses modernc.org/sqlite, a pure-Go SQLite driver, to
// avoid CGO. The store is best-effort: a failure to write never breaks the
// proxy.
//
// Two tables share one DB handle:
//
//   - events (legacy): kind/source/content blobs for the original dump CLI.
//   - requests (v0.2.0+): structured records used by replay, compare,
//     sessions, anomalies, and the Prometheus exporter.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// Event is a row in the legacy `events` table.
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

// ErrNotFound is returned by Get when the requested id does not exist.
var ErrNotFound = errors.New("not found")

// ListFilter narrows a List call. Zero values mean "no filter".
type ListFilter struct {
	Model     string
	Provider  string
	SessionID string
	Since     time.Time
	Until     time.Time
	Limit     int
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

		CREATE TABLE IF NOT EXISTS requests (
			id TEXT PRIMARY KEY,
			started_at TEXT NOT NULL,
			ended_at TEXT,
			method TEXT,
			path TEXT,
			model TEXT,
			upstream TEXT,
			provider TEXT,
			stream INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL DEFAULT 0,
			error TEXT,
			ttft_ms INTEGER NOT NULL DEFAULT 0,
			total_ms INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			request_body TEXT,
			response_body TEXT,
			prompt_hash TEXT,
			session_id TEXT
		);
		CREATE INDEX IF NOT EXISTS requests_started_at ON requests(started_at);
		CREATE INDEX IF NOT EXISTS requests_model ON requests(model);
		CREATE INDEX IF NOT EXISTS requests_status_code ON requests(status_code);
		CREATE INDEX IF NOT EXISTS requests_prompt_hash ON requests(prompt_hash);
		CREATE INDEX IF NOT EXISTS requests_session_id ON requests(session_id);
	`)
	if err != nil {
		return fmt.Errorf("sqlite init: %w", err)
	}
	return nil
}

// Append writes a legacy event row. Errors are returned for the caller to log.
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

// DumpJSON returns all stored events as a JSON array (legacy).
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

// --- v0.2.0 RequestStore API ---

// Insert persists a Request. Duplicate IDs are silently ignored so re-flushing
// the same id (e.g. an at-least-once retry path) does not overwrite history.
// Returns an error when the id is empty (defense against bad callers).
func (s *Store) Insert(r proxy.Request) error {
	if r.ID == "" {
		return errors.New("store: request id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	endedAt := ""
	if !r.EndedAt.IsZero() {
		endedAt = r.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO requests(
		id, started_at, ended_at, method, path, model, upstream, provider,
		stream, status_code, error,
		ttft_ms, total_ms, prompt_tokens, output_tokens, cost_usd,
		request_body, response_body, prompt_hash, session_id
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID,
		r.StartedAt.UTC().Format(time.RFC3339Nano),
		endedAt,
		r.Method, r.Path, r.Model, r.Upstream, r.Provider,
		boolToInt(r.Stream), r.StatusCode, r.Error,
		r.TTFTMillis, r.TotalMillis, r.PromptTokens, r.OutputTokens, r.CostUSD,
		r.RequestBody, r.ResponseBody, r.PromptHash, r.SessionID,
	)
	return err
}

// Get fetches a single Request by id. Returns ErrNotFound if the id is unknown.
func (s *Store) Get(id string) (proxy.Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT
		id, started_at, ended_at, method, path, model, upstream, provider,
		stream, status_code, error,
		ttft_ms, total_ms, prompt_tokens, output_tokens, cost_usd,
		request_body, response_body, prompt_hash, session_id
	FROM requests WHERE id = ?`, id)
	return scanRequest(row)
}

// List returns requests matching the supplied filter, newest first.
func (s *Store) List(f ListFilter) ([]proxy.Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	q := `SELECT
		id, started_at, ended_at, method, path, model, upstream, provider,
		stream, status_code, error,
		ttft_ms, total_ms, prompt_tokens, output_tokens, cost_usd,
		request_body, response_body, prompt_hash, session_id
	FROM requests WHERE 1=1`
	args := []any{}
	if f.Model != "" {
		q += " AND model = ?"
		args = append(args, f.Model)
	}
	if f.Provider != "" {
		q += " AND provider = ?"
		args = append(args, f.Provider)
	}
	if f.SessionID != "" {
		q += " AND session_id = ?"
		args = append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		q += " AND started_at >= ?"
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		q += " AND started_at < ?"
		args = append(args, f.Until.UTC().Format(time.RFC3339Nano))
	}
	q += " ORDER BY started_at DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []proxy.Request
	for rows.Next() {
		r, err := scanRequestRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Recent returns the n most recent requests, newest first.
func (s *Store) Recent(n int) ([]proxy.Request, error) {
	return s.List(ListFilter{Limit: n})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanRequest(row *sql.Row) (proxy.Request, error) {
	var r proxy.Request
	var started, ended string
	var stream int
	err := row.Scan(
		&r.ID, &started, &ended, &r.Method, &r.Path, &r.Model, &r.Upstream, &r.Provider,
		&stream, &r.StatusCode, &r.Error,
		&r.TTFTMillis, &r.TotalMillis, &r.PromptTokens, &r.OutputTokens, &r.CostUSD,
		&r.RequestBody, &r.ResponseBody, &r.PromptHash, &r.SessionID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return proxy.Request{}, ErrNotFound
		}
		return proxy.Request{}, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if ended != "" {
		r.EndedAt, _ = time.Parse(time.RFC3339Nano, ended)
	}
	r.Stream = stream != 0
	return r, nil
}

func scanRequestRows(rows *sql.Rows) (proxy.Request, error) {
	var r proxy.Request
	var started, ended string
	var stream int
	err := rows.Scan(
		&r.ID, &started, &ended, &r.Method, &r.Path, &r.Model, &r.Upstream, &r.Provider,
		&stream, &r.StatusCode, &r.Error,
		&r.TTFTMillis, &r.TotalMillis, &r.PromptTokens, &r.OutputTokens, &r.CostUSD,
		&r.RequestBody, &r.ResponseBody, &r.PromptHash, &r.SessionID,
	)
	if err != nil {
		return proxy.Request{}, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if ended != "" {
		r.EndedAt, _ = time.Parse(time.RFC3339Nano, ended)
	}
	r.Stream = stream != 0
	return r, nil
}
