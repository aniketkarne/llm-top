// Package ring implements a fixed-size circular buffer for capturing recent
// proxy activity (prompts and streamed outputs) without unbounded growth.
//
// The buffer is safe for concurrent use: a single mutex guards the underlying
// slice, which is sufficient for the modest throughput of an interactive TUI.
package ring

import (
	"sync"
	"time"
)

// Entry is a single record stored in the ring buffer. Caller supplies the
// kind, source label, and content. Timestamp is captured at Append time.
type Entry struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`    // "request" or "response"
	Source  string    `json:"source"`  // logical source label, e.g. model name
	Content string    `json:"content"` // body content (already redacted if needed)
}

// Buffer is a fixed-capacity ring of Entries. When the buffer is full, the
// oldest entry is overwritten on the next Append.
type Buffer struct {
	mu      sync.Mutex
	data    []Entry
	cap     int
	head    int  // index of next write slot
	full    bool // true once data has wrapped at least once
	counter uint64
}

// New returns a Buffer with the given capacity. Capacity must be > 0.
func New(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &Buffer{
		data: make([]Entry, capacity),
		cap:  capacity,
	}
}

// Append adds a new entry, overwriting the oldest if at capacity.
func (b *Buffer) Append(kind, source, content string) Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := Entry{
		Time:    time.Now(),
		Kind:    kind,
		Source:  source,
		Content: content,
	}
	b.data[b.head] = e
	b.head = (b.head + 1) % b.cap
	if b.head == 0 {
		b.full = true
	}
	b.counter++
	return e
}

// Len returns the current number of stored entries (<= cap).
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full {
		return b.cap
	}
	return b.head
}

// Cap returns the configured capacity.
func (b *Buffer) Cap() int { return b.cap }

// Snapshot returns a copy of the stored entries in chronological order
// (oldest first). Safe to mutate the returned slice.
func (b *Buffer) Snapshot() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.head
	if b.full {
		n = b.cap
	}
	out := make([]Entry, 0, n)
	if b.full {
		// oldest is at head (next-write slot)
		out = append(out, b.data[b.head:]...)
		out = append(out, b.data[:b.head]...)
	} else {
		out = append(out, b.data[:b.head]...)
	}
	return out
}

// Clear empties the buffer without reallocating.
func (b *Buffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.data {
		b.data[i] = Entry{}
	}
	b.head = 0
	b.full = false
}
