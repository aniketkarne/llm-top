package session

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// fakeStore is an in-memory SessionStore for testing Manager without
// touching SQLite. We copy the contract of internal/store but
// deliberately do NOT enforce the single-active-session invariant
// at the store level for the "concurrent" test — that test exercises
// the invariant via two consecutive Manager.Create calls, which is
// the realistic concurrency shape.
type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]Session
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: map[string]Session{}}
}

func (f *fakeStore) CreateSession(s Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.sessions {
		if existing.EndedAt.IsZero() {
			return ErrSessionActive
		}
	}
	f.sessions[s.ID] = s
	return nil
}

func (f *fakeStore) EndSession(id string, when time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return ErrNotFound
	}
	s.EndedAt = when
	s.Active = false
	f.sessions[id] = s
	return nil
}

func (f *fakeStore) GetSession(id string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return s, nil
}

func (f *fakeStore) ListSessions() ([]Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Session, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, s)
	}
	// newest first — match the SQLite order. Tie-break on ID (which
	// is roughly time-sorted) so the order is deterministic when two
	// sessions share a StartedAt (e.g. created in the same nanosecond
	// during fast unit tests).
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			oi, oj := out[i], out[j]
			if oj.StartedAt.After(oi.StartedAt) ||
				oj.StartedAt.Equal(oi.StartedAt) && oj.ID > oi.ID {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (f *fakeStore) ActiveSession() (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.EndedAt.IsZero() {
			return s, nil
		}
	}
	return Session{}, ErrNoActiveSession
}

// --- Manager.Create ---

func TestManagerCreateSetsDefaults(t *testing.T) {
	m := NewManager(newFakeStore())
	s, err := m.Create(Session{Name: "smoke", Label: "qa"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID == "" || len(s.ID) != 16 {
		t.Errorf("ID should be a 16-hex string, got %q (len %d)", s.ID, len(s.ID))
	}
	if s.StartedAt.IsZero() {
		t.Errorf("StartedAt should be populated")
	}
	if !s.EndedAt.IsZero() {
		t.Errorf("EndedAt should be zero on create, got %v", s.EndedAt)
	}
	if !s.Active {
		t.Errorf("Active should be true on create")
	}
}

func TestManagerCreateConcurrentFails(t *testing.T) {
	m := NewManager(newFakeStore())
	if _, err := m.Create(Session{}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := m.Create(Session{})
	if !errors.Is(err, ErrSessionActive) {
		t.Errorf("second Create while active: want ErrSessionActive, got %v", err)
	}
}

func TestManagerCreateAfterEndSucceeds(t *testing.T) {
	m := NewManager(newFakeStore())
	first, _ := m.Create(Session{})
	if err := m.End(first.ID, time.Now().UTC()); err != nil {
		t.Fatalf("End: %v", err)
	}
	second, err := m.Create(Session{})
	if err != nil {
		t.Fatalf("Create after End: %v", err)
	}
	if second.ID == first.ID {
		t.Errorf("second session should have a fresh id, got duplicate %s", second.ID)
	}
}

// --- Manager.End ---

func TestManagerEndMarksEndedAt(t *testing.T) {
	m := NewManager(newFakeStore())
	s, _ := m.Create(Session{})
	when := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	if err := m.End(s.ID, when); err != nil {
		t.Fatalf("End: %v", err)
	}
	got, _ := m.Get(s.ID)
	if !got.EndedAt.Equal(when) {
		t.Errorf("EndedAt: want %v, got %v", when, got.EndedAt)
	}
	if got.Active {
		t.Errorf("Active should be false after End")
	}
}

func TestManagerEndDefaultsToNow(t *testing.T) {
	m := NewManager(newFakeStore())
	s, _ := m.Create(Session{})
	before := time.Now().UTC()
	if err := m.End(s.ID, time.Time{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	got, _ := m.Get(s.ID)
	if got.EndedAt.Before(before) {
		t.Errorf("EndedAt should default to ~now: got %v, before was %v", got.EndedAt, before)
	}
}

func TestManagerEndAlreadyEndedReturnsError(t *testing.T) {
	m := NewManager(newFakeStore())
	s, _ := m.Create(Session{})
	if err := m.End(s.ID, time.Now().UTC()); err != nil {
		t.Fatalf("first End: %v", err)
	}
	err := m.End(s.ID, time.Now().UTC())
	if !errors.Is(err, ErrAlreadyEnded) {
		t.Errorf("second End: want ErrAlreadyEnded, got %v", err)
	}
}

func TestManagerEndEmptyIDRejected(t *testing.T) {
	m := NewManager(newFakeStore())
	if err := m.End("", time.Now().UTC()); !errors.Is(err, ErrEmptyID) {
		t.Errorf("empty id: want ErrEmptyID, got %v", err)
	}
}

func TestManagerEndUnknownIDReturnsNotFound(t *testing.T) {
	m := NewManager(newFakeStore())
	err := m.End("doesnotexist", time.Now().UTC())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: want ErrNotFound, got %v", err)
	}
}

// --- Manager.Get / Active / FindByName ---

func TestManagerGetRoundtrip(t *testing.T) {
	m := NewManager(newFakeStore())
	created, _ := m.Create(Session{Name: "baseline", Label: "perf"})
	got, err := m.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("id mismatch: want %s, got %s", created.ID, got.ID)
	}
	if got.Name != "baseline" || got.Label != "perf" {
		t.Errorf("metadata lost: %+v", got)
	}
}

func TestManagerActive(t *testing.T) {
	m := NewManager(newFakeStore())
	_, err := m.Active()
	if !errors.Is(err, ErrNoActiveSession) {
		t.Errorf("no sessions: want ErrNoActiveSession, got %v", err)
	}
	s, _ := m.Create(Session{})
	got, err := m.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("active id: want %s, got %s", s.ID, got.ID)
	}
	_ = m.End(s.ID, time.Now().UTC())
	_, err = m.Active()
	if !errors.Is(err, ErrNoActiveSession) {
		t.Errorf("after End: want ErrNoActiveSession, got %v", err)
	}
}

func TestManagerFindByNameAndID(t *testing.T) {
	m := NewManager(newFakeStore())
	created, _ := m.Create(Session{Name: "my-baseline"})

	// Look up by ID.
	got, err := m.FindByName(created.ID)
	if err != nil || got.ID != created.ID {
		t.Errorf("find by id: err=%v got=%+v", err, got)
	}
	// Look up by name.
	got, err = m.FindByName("my-baseline")
	if err != nil || got.ID != created.ID {
		t.Errorf("find by name: err=%v got=%+v", err, got)
	}
	// Unknown.
	_, err = m.FindByName("does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: want ErrNotFound, got %v", err)
	}
}

// --- Manager.List ---

func TestManagerListNewestFirst(t *testing.T) {
	m := NewManager(newFakeStore())
	a, _ := m.Create(Session{Name: "a"})
	_ = m.End(a.ID, time.Now().UTC())
	b, _ := m.Create(Session{Name: "b"})
	_ = m.End(b.ID, time.Now().UTC())
	c, _ := m.Create(Session{Name: "c"})
	list, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3, got %d", len(list))
	}
	if list[0].ID != c.ID {
		t.Errorf("newest first: got %s, want %s", list[0].ID, c.ID)
	}
}

// --- Manager.Aggregate ---

func TestManagerAggregateComputesStats(t *testing.T) {
	m := NewManager(newFakeStore())
	s, _ := m.Create(Session{})
	defer m.End(s.ID, time.Now().UTC())

	// fakeListFn returns two requests for the session id, one good
	// and one errored. The Manager doesn't care which other session
	// IDs the list contains — it just delegates.
	requests := []proxy.Request{
		mkReq("a", true, 100, 1000, 10, 20, 0.001, 200, ""),
		mkReq("b", true, 300, 3000, 10, 20, 0.001, 500, "upstream error"),
	}
	stats, err := m.Aggregate(s.ID, func(id string) ([]proxy.Request, error) {
		if id != s.ID {
			t.Errorf("listFn called with wrong id: %s", id)
		}
		return requests, nil
	})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Count != 2 || stats.ErrorCount != 1 {
		t.Errorf("stats wrong: %+v", stats)
	}
	if stats.TotalInputTokens != 20 {
		t.Errorf("input tokens: %d", stats.TotalInputTokens)
	}
}

func TestManagerAggregateUnknownSessionErrors(t *testing.T) {
	m := NewManager(newFakeStore())
	_, err := m.Aggregate("nope", func(string) ([]proxy.Request, error) {
		return nil, nil
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: want ErrNotFound, got %v", err)
	}
}

// --- Nil manager ---

func TestNilManagerReturnsErrors(t *testing.T) {
	var m *Manager
	if _, err := m.Create(Session{}); err == nil {
		t.Errorf("nil manager Create should error")
	}
	if err := m.End("x", time.Time{}); err == nil {
		t.Errorf("nil manager End should error")
	}
	if _, err := m.Get("x"); err == nil {
		t.Errorf("nil manager Get should error")
	}
}

// --- Tags serialization (internal helper) ---

func TestTagsRoundtrip(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "[]"},
		{[]string{}, "[]"},
		{[]string{"one"}, `["one"]`},
		{[]string{"one", "two"}, `["one","two"]`},
	}
	for _, c := range cases {
		got, err := tagsToJSON(c.in)
		if err != nil {
			t.Errorf("tagsToJSON(%v): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("tagsToJSON(%v): want %s, got %s", c.in, c.want, got)
		}
		// round-trip — both nil and empty slice become "[]" on the
		// wire; json.Unmarshal of "[]" yields a non-nil zero-length
	// slice, which we treat as equivalent.
	parsed := tagsFromJSON(got)
		if len(parsed) != len(c.in) {
			t.Errorf("tagsFromJSON round-trip: want len %d, got %d (in=%v parsed=%v)", len(c.in), len(parsed), c.in, parsed)
		}
		for i := range parsed {
			if i < len(c.in) && parsed[i] != c.in[i] {
				t.Errorf("tagsFromJSON[%d]: want %q, got %q", i, c.in[i], parsed[i])
			}
		}
	}
}

func TestTagsFromJSONTolerantOfBadInput(t *testing.T) {
	// Malformed JSON should not panic — return nil so reads succeed
	// even when the stored value is corrupt.
	if got := tagsFromJSON("not-json"); got != nil {
		t.Errorf("bad JSON: want nil, got %v", got)
	}
	if got := tagsFromJSON(""); got != nil {
		t.Errorf("empty: want nil, got %v", got)
	}
}

// --- Concurrent session ID generation ---

func TestConcurrentManagerCreates(t *testing.T) {
	// With a serializing fake store, only the first Create in any
	// window should succeed. We model the realistic concurrent
	// scenario: many goroutines call Create; all but one should see
	// ErrSessionActive, then the winner ends, then another wave can
	// proceed.
	m := NewManager(newFakeStore())
	first, err := m.Create(Session{})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	const N = 50
	var wg sync.WaitGroup
	results := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := m.Create(Session{})
			results[idx] = err
		}(i)
	}
	wg.Wait()
	for _, err := range results {
		if !errors.Is(err, ErrSessionActive) {
			t.Errorf("concurrent Create: want ErrSessionActive, got %v", err)
		}
	}
	// Now end and confirm a new Create works.
	if err := m.End(first.ID, time.Now().UTC()); err != nil {
		t.Fatalf("End: %v", err)
	}
	if _, err := m.Create(Session{}); err != nil {
		t.Errorf("Create after End: %v", err)
	}
}