//go:build !sqlite

// Package store provides a no-op fallback when SQLite support is not compiled in.
// To enable SQLite, build with:  go build -tags sqlite
package store

import "errors"

// Event mirrors the SQLite build's row shape.
type Event struct {
	Time    interface{}
	Kind    string
	Model   string
	Content string
}

// Store is a no-op implementation.
type Store struct{}

// Open returns ErrUnavailable because SQLite is not compiled in.
func Open(path string) (*Store, error) {
	return nil, errors.New("sqlite support not compiled in; build with -tags sqlite")
}

// Append is a no-op.
func (s *Store) Append(e Event) error { return nil }

// Close is a no-op.
func (s *Store) Close() error { return nil }

// DumpJSON is a no-op.
func (s *Store) DumpJSON() ([]byte, error) { return []byte("[]"), nil }
