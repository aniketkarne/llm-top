package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// Session is a logical bucket of proxied requests. Sessions have a
// lifecycle (StartedAt..EndedAt), human-readable metadata (Name,
// Label, Tags), and are linked one-to-many with proxy.Request records
// via Request.SessionID.
//
// The store persists Sessions in the `sessions` table; the Manager
// in this file is the typed entry point. Aggregate over a Session's
// requests via session.Aggregate (see aggregate.go).
//
// Field semantics:
//   - ID: 16-hex-char identifier, same shape as proxy.Request.ID. The
//     Manager generates a new one in Create.
//   - Name: optional human label ("baseline-2026-09-13").
//   - Label: optional one-word tag ("perf", "smoke").
//   - Tags: free-form list, persisted as JSON in the SQLite tags
//     column for forward compatibility.
//   - StartedAt / EndedAt: UTC. EndedAt is the zero value when the
//     session is still active.
//   - Active is computed (EndedAt.IsZero()) and not stored.
//   - RequestCount / ErrorCount / TotalCostUSD / TotalTokens are
//     populated by the Manager's Aggregate helper, not from the
//     sessions row directly — they describe the linked requests, not
//     fields on the row itself.
type Session struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Label         string    `json:"label,omitempty"`
	Tags          []string  `json:"tags,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	Active        bool      `json:"active"`
	RequestCount  int       `json:"request_count"`
	ErrorCount   int        `json:"error_count"`
	TotalCostUSD  float64   `json:"total_cost_usd"`
	TotalTokens   int       `json:"total_tokens"`
}

// SessionStore is the persistence interface the Manager depends on.
// The concrete implementation in internal/store satisfies it for
// the sqlite build; the no-op fallback satisfies it for the default
// build (returning ErrUnavailable from every method).
//
// Defining it here keeps the session package free of an import on
// internal/store, and lets tests fake the persistence layer with an
// in-memory map.
type SessionStore interface {
	CreateSession(s Session) error
	EndSession(id string, when time.Time) error
	GetSession(id string) (Session, error)
	ListSessions() ([]Session, error)
	ActiveSession() (Session, error)
}

// Errors returned by Manager methods.
var (
	// ErrSessionActive is returned by Create when a session with
	// EndedAt == zero is already present. v0.2.0 supports one active
	// session at a time per database; ending the current one is the
	// caller's responsibility.
	ErrSessionActive = errors.New("session: another session is already active; end it first")

	// ErrNoActiveSession is returned by ActiveSession when there is
	// no session with EndedAt == zero in the store.
	ErrNoActiveSession = errors.New("session: no active session")

	// ErrNotFound is returned by Get/End when the requested id is
	// unknown. Wrapped to match store.ErrNotFound for compatibility
	// with errors.Is callers.
	ErrNotFound = errors.New("session: not found")

	// ErrEmptyID is returned by Manager methods when the supplied id
	// is empty. Defense against accidental zero-value passes.
	ErrEmptyID = errors.New("session: id is empty")

	// ErrAlreadyEnded is returned by End when the target session is
	// already in the ended state. We treat this as a no-op-success
	// in Manager.End so the CLI's repeated `session end` is not a
	// hard error, but expose the underlying error for callers that
	// want to distinguish.
	ErrAlreadyEnded = errors.New("session: already ended")
)

// Manager is the public API for session lifecycle operations. It is
// safe for concurrent use — the underlying store implementation
// (internal/store) takes its own mutex; the Manager itself holds no
// mutable state.
type Manager struct {
	store SessionStore
}

// NewManager constructs a Manager bound to the given SessionStore.
// Returns nil when store is nil — callers should treat a nil Manager
// as "session features disabled" rather than a hard error.
func NewManager(store SessionStore) *Manager {
	if store == nil {
		return nil
	}
	return &Manager{store: store}
}

// Create persists a new Session. The supplied Session's ID, StartedAt,
// EndedAt fields are overwritten:
//
//   - ID is set to a fresh 16-hex-char identifier (same generator
//     as proxy.Request.NewID).
//   - StartedAt is set to time.Now().UTC() unless the caller already
//     populated a non-zero value (back-compat for batch imports).
//   - EndedAt is cleared.
//
// Returns ErrSessionActive if another session is currently active
// (EndedAt == zero in the store). Use ActiveSession to discover the
// existing id, then End it before creating a new one.
func (m *Manager) Create(s Session) (Session, error) {
	if m == nil {
		return Session{}, errors.New("session: manager is nil")
	}
	if existing, err := m.store.ActiveSession(); err == nil && !existing.EndedAt.IsZero() == false {
		// existing.EndedAt is zero -> still active.
		return Session{}, ErrSessionActive
	} else if err != nil && !errors.Is(err, ErrNoActiveSession) {
		return Session{}, err
	}
	s.ID = proxy.NewID()
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now().UTC()
	}
	s.EndedAt = time.Time{}
	s.Active = true
	if err := m.store.CreateSession(s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// End marks the session with the supplied id as ended at the
// supplied time. If when.IsZero() it defaults to time.Now().UTC().
//
// End is idempotent: calling End on an already-ended session
// returns ErrAlreadyEnded without modifying the row. Callers who
// prefer a no-op can ignore that error — it's surfaced for tests
// and for callers that want to react (e.g. printing a warning).
func (m *Manager) End(id string, when time.Time) error {
	if m == nil {
		return errors.New("session: manager is nil")
	}
	if id == "" {
		return ErrEmptyID
	}
	if when.IsZero() {
		when = time.Now().UTC()
	}
	if existing, err := m.store.GetSession(id); err != nil {
		return err
	} else if !existing.EndedAt.IsZero() {
		return ErrAlreadyEnded
	}
	return m.store.EndSession(id, when)
}

// Get returns the Session with the given id, or ErrNotFound.
// The returned Session's RequestCount / ErrorCount / TotalCostUSD /
// TotalTokens fields are NOT populated here — they are zero. Use
// Aggregate(sessionID) to compute them from the linked Requests.
func (m *Manager) Get(id string) (Session, error) {
	if m == nil {
		return Session{}, errors.New("session: manager is nil")
	}
	if id == "" {
		return Session{}, ErrEmptyID
	}
	return m.store.GetSession(id)
}

// Active returns the currently-active session (EndedAt == zero), or
// ErrNoActiveSession. There is at most one active session per store.
func (m *Manager) Active() (Session, error) {
	if m == nil {
		return Session{}, errors.New("session: manager is nil")
	}
	return m.store.ActiveSession()
}

// List returns all sessions, newest first. The concrete store
// implementation is responsible for ordering — Manager does not
// re-sort.
func (m *Manager) List() ([]Session, error) {
	if m == nil {
		return nil, errors.New("session: manager is nil")
	}
	return m.store.ListSessions()
}

// FindByName resolves a session identifier supplied on the CLI —
// either a 16-hex ID or a human name. We try the ID first, then
// fall back to a case-sensitive name match. Returns ErrNotFound
// when neither resolves.
//
// Names are not unique (the schema allows duplicates), so when
// multiple sessions share a name we return the most recently
// started one.
func (m *Manager) FindByName(name string) (Session, error) {
	if m == nil {
		return Session{}, errors.New("session: manager is nil")
	}
	if name == "" {
		return Session{}, ErrEmptyID
	}
	if s, err := m.store.GetSession(name); err == nil {
		return s, nil
	}
	all, err := m.store.ListSessions()
	if err != nil {
		return Session{}, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
	}
	return Session{}, ErrNotFound
}

// Aggregate computes AggregateStats for the session's linked
// requests. It joins on the store to fetch requests by session id,
// then delegates to the pure Aggregate function in aggregate.go.
//
// Returns an error when the session id is unknown OR when the
// underlying request-list query fails. An empty request slice is
// not an error — AggregateStats with Count == 0 is returned.
func (m *Manager) Aggregate(id string, listFn func(sessionID string) ([]proxy.Request, error)) (AggregateStats, error) {
	if m == nil {
		return AggregateStats{}, errors.New("session: manager is nil")
	}
	if id == "" {
		return AggregateStats{}, ErrEmptyID
	}
	if _, err := m.store.GetSession(id); err != nil {
		return AggregateStats{}, err
	}
	requests, err := listFn(id)
	if err != nil {
		return AggregateStats{}, err
	}
	return Aggregate(requests), nil
}

// tagsToJSON serializes a Tags slice for SQLite persistence. We
// persist as a JSON array (not comma-separated) to support tags
// that contain commas. Exported as a helper for tests.
func tagsToJSON(tags []string) (string, error) {
	if tags == nil {
		return "[]", nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("session: marshal tags: %w", err)
	}
	return string(b), nil
}

// tagsFromJSON is the inverse of tagsToJSON. A malformed JSON value
// is treated as "no tags" rather than a hard error so a corrupted
// database does not block reads.
func tagsFromJSON(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}