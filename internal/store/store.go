//go:build !sqlite

// Package store provides a no-op fallback when SQLite support is not compiled in.
// To enable SQLite, build with:  go build -tags sqlite
//
// The fallback exposes the same RequestStore-style API as the SQLite build
// (Insert, Get, List, Recent, ListFilter, ErrNotFound, ErrUnavailable) so
// callers compile against one shape regardless of build tag.
package store

import (
	"errors"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
	"github.com/aniketkarne-com/llm-top/internal/session"
)

// Session mirrors session.Session on the sqlite build so callers can
// write store.Session without depending on the session package
// directly.
type Session = session.Session

// Event mirrors the SQLite build's row shape (legacy API).
type Event struct {
	Time    interface{}
	Kind    string
	Model   string
	Content string
}

// Store is a no-op implementation.
type Store struct{}

// ErrNotFound is returned by Get when the requested id does not exist.
var ErrNotFound = errors.New("not found")

// ErrUnavailable indicates that the requested operation needs the sqlite
// build tag and it was not enabled at compile time.
var ErrUnavailable = errors.New("sqlite support not compiled in; build with -tags sqlite")

// ListFilter narrows a List call. Zero values mean "no filter".
type ListFilter struct {
	Model     string
	Provider  string
	SessionID string
	Since     interface{}
	Until     interface{}
	Limit     int
}

// Open returns ErrUnavailable because SQLite is not compiled in.
func Open(path string) (*Store, error) {
	return nil, ErrUnavailable
}

// Append is a no-op (legacy).
func (s *Store) Append(e Event) error { return nil }

// Close is a no-op.
func (s *Store) Close() error { return nil }

// DumpJSON is a no-op (legacy).
func (s *Store) DumpJSON() ([]byte, error) { return []byte("[]"), nil }

// Insert is unavailable without the sqlite tag.
func (s *Store) Insert(r proxy.Request) error { return ErrUnavailable }

// Get is unavailable without the sqlite tag.
func (s *Store) Get(id string) (proxy.Request, error) { return proxy.Request{}, ErrUnavailable }

// List is unavailable without the sqlite tag.
func (s *Store) List(f ListFilter) ([]proxy.Request, error) { return nil, ErrUnavailable }

// Recent is unavailable without the sqlite tag.
func (s *Store) Recent(n int) ([]proxy.Request, error) { return nil, ErrUnavailable }

// CreateSession is unavailable without the sqlite tag.
func (s *Store) CreateSession(sess Session) error { return ErrUnavailable }

// EndSession is unavailable without the sqlite tag.
func (s *Store) EndSession(id string, when time.Time) error { return ErrUnavailable }

// GetSession is unavailable without the sqlite tag.
func (s *Store) GetSession(id string) (Session, error) { return Session{}, ErrUnavailable }

// ListSessions is unavailable without the sqlite tag.
func (s *Store) ListSessions() ([]Session, error) { return nil, ErrUnavailable }

// ActiveSession is unavailable without the sqlite tag.
func (s *Store) ActiveSession() (Session, error) { return Session{}, ErrUnavailable }
