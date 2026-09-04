package ring

import (
	"sync"
	"testing"
)

func TestNewDefaultsAndCapacity(t *testing.T) {
	b := New(0)
	if b.Cap() != 1 {
		t.Fatalf("default capacity: want 1, got %d", b.Cap())
	}
	b2 := New(5)
	if b2.Cap() != 5 {
		t.Fatalf("capacity: want 5, got %d", b2.Cap())
	}
}

func TestAppendUnderCapacity(t *testing.T) {
	b := New(3)
	b.Append("request", "m", "a")
	b.Append("response", "m", "b")
	if got := b.Len(); got != 2 {
		t.Fatalf("len: want 2, got %d", got)
	}
	snap := b.Snapshot()
	if len(snap) != 2 || snap[0].Content != "a" || snap[1].Content != "b" {
		t.Fatalf("snapshot order wrong: %+v", snap)
	}
}

func TestAppendOverwritesOldest(t *testing.T) {
	b := New(3)
	b.Append("request", "m", "a")
	b.Append("request", "m", "b")
	b.Append("request", "m", "c")
	b.Append("request", "m", "d") // overwrites "a"
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len: want 3, got %d", len(snap))
	}
	want := []string{"b", "c", "d"}
	for i, w := range want {
		if snap[i].Content != w {
			t.Fatalf("snap[%d]: want %q, got %q", i, w, snap[i].Content)
		}
	}
}

func TestClear(t *testing.T) {
	b := New(2)
	b.Append("request", "m", "x")
	b.Append("response", "m", "y")
	b.Clear()
	if b.Len() != 0 {
		t.Fatalf("len after clear: want 0, got %d", b.Len())
	}
}

func TestConcurrentAppendSafe(t *testing.T) {
	b := New(100)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Append("request", "m", "x")
		}()
	}
	wg.Wait()
	if b.Len() != 100 {
		t.Fatalf("len after concurrent: want 100 (cap), got %d", b.Len())
	}
}
