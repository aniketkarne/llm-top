//go:build sqlite

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/session"
)

// mkSession returns a Session with a populated ID + StartedAt so we
// don't have to repeat the boilerplate in every test case.
func mkSession(id, name, label string, tags []string) session.Session {
	return session.Session{
		ID:        id,
		Name:      name,
		Label:     label,
		Tags:      tags,
		StartedAt: time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC),
	}
}

func TestSessionCreateAndGet(t *testing.T) {
	s := openTemp(t)
	sess := mkSession("0123456789abcdef", "baseline", "perf", []string{"release"})
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession("0123456789abcdef")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Name != "baseline" || got.Label != "perf" {
		t.Errorf("metadata lost: %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "release" {
		t.Errorf("tags lost: %+v", got.Tags)
	}
	if !got.Active {
		t.Errorf("Active should be true before End")
	}
	if got.StartedAt.IsZero() {
		t.Errorf("StartedAt should be populated")
	}
}

func TestSessionEmptyIDRejected(t *testing.T) {
	s := openTemp(t)
	if err := s.CreateSession(session.Session{}); err == nil {
		t.Fatal("CreateSession with empty id should error")
	}
}

func TestSessionOnlyOneActive(t *testing.T) {
	s := openTemp(t)
	if err := s.CreateSession(mkSession("a", "a", "", nil)); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.CreateSession(mkSession("b", "b", "", nil))
	if !errors.Is(err, session.ErrSessionActive) {
		t.Errorf("second while active: want ErrSessionActive, got %v", err)
	}
	// End the first; the second should now succeed.
	when := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
	if err := s.EndSession("a", when); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := s.CreateSession(mkSession("b", "b", "", nil)); err != nil {
		t.Fatalf("Create after End: %v", err)
	}
}

func TestSessionEndAndActive(t *testing.T) {
	s := openTemp(t)
	if err := s.CreateSession(mkSession("a", "", "", nil)); err != nil {
		t.Fatal(err)
	}
	// ActiveSession should find it.
	active, err := s.ActiveSession()
	if err != nil {
		t.Fatalf("ActiveSession: %v", err)
	}
	if active.ID != "a" {
		t.Errorf("active id: want a, got %s", active.ID)
	}
	when := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	if err := s.EndSession("a", when); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	// ActiveSession should now return ErrNoActiveSession.
	_, err = s.ActiveSession()
	if !errors.Is(err, session.ErrNoActiveSession) {
		t.Errorf("after End: want ErrNoActiveSession, got %v", err)
	}
	// Get should still find it, with EndedAt populated.
	got, _ := s.GetSession("a")
	if !got.EndedAt.Equal(when) {
		t.Errorf("EndedAt: want %v, got %v", when, got.EndedAt)
	}
	if got.Active {
		t.Errorf("Active should be false after End")
	}
}

func TestSessionEndUnknownIDReturnsNotFound(t *testing.T) {
	s := openTemp(t)
	err := s.EndSession("nonexistent", time.Now().UTC())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestSessionListOrder(t *testing.T) {
	s := openTemp(t)
	base := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{"first", "second", "third"} {
		sess := mkSession(id, id, "", nil)
		sess.StartedAt = base.Add(time.Duration(id[0]-'f') * time.Minute)
		if err := s.CreateSession(sess); err != nil {
			// Skip ErrSessionActive for the 2nd and 3rd by ending
			// the previous one first.
			if errors.Is(err, session.ErrSessionActive) {
				if eid := prevID(id); eid != "" {
					_ = s.EndSession(eid, base)
				}
				if err := s.CreateSession(sess); err != nil {
					t.Fatalf("retry %s: %v", id, err)
				}
			} else {
				t.Fatal(err)
			}
		}
	}
	list, err := s.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3, got %d", len(list))
	}
	// Newest first: "third", "second", "first".
	if list[0].ID != "third" || list[1].ID != "second" || list[2].ID != "first" {
		t.Errorf("ordering wrong: %v %v %v", list[0].ID, list[1].ID, list[2].ID)
	}
}

// prevID returns the ID lexicographically just before the given one
// in our test ordering ("first" -> "" since it's first; "second" ->
// "first"; "third" -> "second"). Used to end the previous session
// before inserting the next.
func prevID(id string) string {
	switch id {
	case "second":
		return "first"
	case "third":
		return "second"
	}
	return ""
}

func TestSessionTagsRoundtrip(t *testing.T) {
	s := openTemp(t)
	cases := [][]string{
		nil,
		{},
		{"one"},
		{"with spaces", "and,commas"},
		{"unicode-éñ"},
	}
	prevID := ""
	for i, tags := range cases {
		id := "id" + string(rune('a'+i))
		sess := mkSession(id, "", "", tags)
		err := s.CreateSession(sess)
		if errors.Is(err, session.ErrSessionActive) {
			// Only the first session can be active at a time; end the
			// previous one and retry.
			if prevID == "" {
				t.Fatalf("retry %d: no previous session to end", i)
			}
			if e := s.EndSession(prevID, time.Now().UTC()); e != nil {
				t.Fatalf("end prev %q: %v", prevID, e)
			}
			err = s.CreateSession(sess)
		}
		if err != nil {
			t.Fatalf("CreateSession %d: %v", i, err)
		}
		prevID = id
		got, _ := s.GetSession(id)
		if len(got.Tags) != len(tags) {
			t.Errorf("tags roundtrip %d: want %d, got %d (%v)", i, len(tags), len(got.Tags), got.Tags)
		}
	}
}

func TestSessionPersistsAcrossReopen(t *testing.T) {
	// Sessions survive store close/reopen. We re-open the same path
	// and confirm the row is still there.
	dir := t.TempDir()
	path := dir + "/persist.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.CreateSession(mkSession("persist", "p", "", nil)); err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetSession("persist")
	if err != nil {
		t.Fatalf("GetSession after reopen: %v", err)
	}
	if got.Name != "p" {
		t.Errorf("lost name across reopen: %+v", got)
	}
}