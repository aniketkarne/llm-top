package ring

import (
	"sync"
	"testing"
)

// --- Hardening: edge cases for the ring buffer ---

func TestNewClampsZeroCapacity(t *testing.T) {
	// Capacity <= 0 should not panic and should still be usable.
	b := New(0)
	if b.Cap() < 1 {
		t.Errorf("New(0).Cap(): want >= 1, got %d", b.Cap())
	}
	b.Append("test", "src", "content")
	if b.Len() != 1 {
		t.Errorf("Len after one Append: want 1, got %d", b.Len())
	}
}

func TestNewClampsNegativeCapacity(t *testing.T) {
	b := New(-10)
	if b.Cap() < 1 {
		t.Errorf("New(-10).Cap(): want >= 1, got %d", b.Cap())
	}
}

func TestWrapAroundPreservesOrder(t *testing.T) {
	// Fill past capacity and verify Snapshot is in chronological order.
	b := New(3)
	for i := 0; i < 10; i++ {
		b.Append("k", "src", string(rune('a'+i)))
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot length: want 3, got %d", len(snap))
	}
	// After 10 appends with cap 3, the last 3 should be 'h', 'i', 'j'.
	// (entries 0..9 = 'a'..'j', kept entries are indices 7, 8, 9).
	want := []string{"h", "i", "j"}
	for i, e := range snap {
		if e.Content != want[i] {
			t.Errorf("snap[%d].Content: want %q, got %q", i, want[i], e.Content)
		}
	}
}

func TestClearResetsState(t *testing.T) {
	b := New(5)
	for i := 0; i < 5; i++ {
		b.Append("k", "src", "content")
	}
	b.Clear()
	if b.Len() != 0 {
		t.Errorf("Len after Clear: want 0, got %d", b.Len())
	}
	snap := b.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot length after Clear: want 0, got %d", len(snap))
	}
	// Should accept new appends.
	b.Append("k", "src", "new")
	if b.Len() != 1 {
		t.Errorf("Len after Append following Clear: want 1, got %d", b.Len())
	}
}

func TestConcurrentAppendRaceSafe(t *testing.T) {
	// Race detector smoke test: many concurrent appends must not corrupt
	// the buffer. Run with `go test -race`.
	b := New(100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Append("k", "src", string(rune('a'+(i%26))))
		}(i)
	}
	wg.Wait()
	if b.Len() > 100 {
		t.Errorf("buffer overflowed capacity: len=%d cap=%d", b.Len(), b.Cap())
	}
}

func TestSnapshotIndependentFromBuffer(t *testing.T) {
	// Modifying the snapshot slice must not affect the buffer.
	b := New(5)
	b.Append("k", "src", "first")
	snap := b.Snapshot()
	snap[0].Content = "MUTATED"
	snap2 := b.Snapshot()
	if snap2[0].Content != "first" {
		t.Errorf("snapshot leak: snap2 = %q, want %q", snap2[0].Content, "first")
	}
}

func TestSnapshotOnEmptyBuffer(t *testing.T) {
	b := New(10)
	snap := b.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot on empty: want non-nil slice, got nil")
	}
	if len(snap) != 0 {
		t.Errorf("Snapshot on empty: want length 0, got %d", len(snap))
	}
}

func TestAppendReturnsEntry(t *testing.T) {
	// The Entry returned by Append should reflect what was stored.
	b := New(3)
	e := b.Append("request", "gpt-4o", "hello")
	if e.Kind != "request" || e.Source != "gpt-4o" || e.Content != "hello" {
		t.Errorf("returned Entry wrong: %+v", e)
	}
	if e.Time.IsZero() {
		t.Errorf("returned Entry Time should not be zero")
	}
}