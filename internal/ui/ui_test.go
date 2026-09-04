package ui

import (
	"strings"
	"testing"
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
)

type fakeStats struct{ total, errors uint64 }

func (f fakeStats) Stats() (uint64, uint64) { return f.total, f.errors }

func TestRenderSnapshotEmpty(t *testing.T) {
	rec := metrics.NewRecorder()
	rb := ring.New(10)
	m := New(rec, rb, fakeStats{}, "127.0.0.1:7777", "https://api.openai.com")
	m.Width = 80
	out := m.RenderSnapshot()
	if !strings.Contains(out, "llm-top") {
		t.Fatal("missing title")
	}
	if !strings.Contains(out, "no traffic yet") {
		t.Fatal("expected empty-state message")
	}
	if !strings.Contains(out, "127.0.0.1:7777") {
		t.Fatal("expected listen addr")
	}
}

func TestRenderSnapshotWithEntries(t *testing.T) {
	rec := metrics.NewRecorder()
	rec.Add(metrics.Record{Status: 200, OutputTok: 7, PromptTok: 3, Total: 120 * time.Millisecond, TTFT: 30 * time.Millisecond})
	rec.Add(metrics.Record{Status: 500, Err: "boom", Total: 50 * time.Millisecond})
	rb := ring.New(20)
	rb.Append("request", "gpt-4", `{"model":"gpt-4"}`)
	rb.Append("response", "gpt-4", `{"choices":[{"message":{"content":"hi"}}]}`)
	m := New(rec, rb, fakeStats{total: 2, errors: 1}, "127.0.0.1:7777", "https://api.openai.com")
	m.Width = 100
	out := m.RenderSnapshot()
	for _, want := range []string{"gpt-4", "avg TTFT", "p95 total", "requests      : 2", "errors        : 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in snapshot", want)
		}
	}
}

func TestColorDisabled(t *testing.T) {
	rec := metrics.NewRecorder()
	rb := ring.New(10)
	m := New(rec, rb, fakeStats{}, ":0", "")
	m.NoColor = true
	out := m.RenderSnapshot()
	if strings.Contains(out, "\033[") {
		t.Fatalf("ANSI escapes should be off, got %q", out)
	}
}

func TestPadAndTruncate(t *testing.T) {
	if padRight("hi", 5) != "hi   " {
		t.Fatal("padRight failed")
	}
	if truncate("hello world", 5) != "hell…" {
		t.Fatalf("truncate got %q", truncate("hello world", 5))
	}
}
