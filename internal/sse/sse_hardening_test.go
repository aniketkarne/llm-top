package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// --- Hardening: malformed SSE inputs ---

func TestParseMalformedMissingBlankLine(t *testing.T) {
	// Two events glued together without the blank-line separator.
	// The parser must not panic and must return one event, then continue.
	in := "data: hello\ndata: world\n\n"
	p := New(strings.NewReader(in))
	ev, err := p.NextEvent()
	if err != nil {
		t.Fatalf("first err: %v", err)
	}
	if !strings.Contains(ev.Data, "hello") {
		t.Errorf("first event data: want contains 'hello', got %q", ev.Data)
	}
	// Drain remainder.
	for {
		_, err := p.NextEvent()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("unexpected err after first event: %v", err)
		}
	}
}

func TestParseHugePayload(t *testing.T) {
	// 5 MB of 'a' inside one data line — exercises the scanner.
	huge := strings.Repeat("a", 5*1024*1024)
	p := New(strings.NewReader("data: "+huge+"\n\n"))
	ev, err := p.NextEvent()
	if err != nil {
		t.Fatalf("err on huge payload: %v", err)
	}
	if len(ev.Data) != len(huge) {
		t.Errorf("data length: want %d, got %d", len(huge), len(ev.Data))
	}
}

func TestParseUnicodePayload(t *testing.T) {
	// Multi-byte unicode (emoji + CJK + RTL).
	unicode := "🚀 こんにちは العالم مرحبا Ω ≠ π"
	p := New(strings.NewReader("data: "+unicode+"\n\n"))
	ev, err := p.NextEvent()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Data != unicode {
		t.Errorf("unicode roundtrip: want %q, got %q", unicode, ev.Data)
	}
}

func TestParseCarriageReturnLineEndings(t *testing.T) {
	// SSE spec allows \r\n; verify the parser tolerates it.
	in := "data: hello\r\n\r\n"
	p := New(strings.NewReader(in))
	ev, err := p.NextEvent()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Data != "hello" {
		t.Errorf("data: want 'hello', got %q", ev.Data)
	}
}

func TestParseMultipleDataLinesConcatenated(t *testing.T) {
	// Multi-line data: lines after the first 'data:' are appended with
	// newlines per the SSE spec.
	in := "data: line1\ndata: line2\ndata: line3\n\n"
	p := New(strings.NewReader(in))
	ev, err := p.NextEvent()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := "line1\nline2\nline3"
	if ev.Data != want {
		t.Errorf("multi-line data: want %q, got %q", want, ev.Data)
	}
}

func TestParseEmptyStream(t *testing.T) {
	p := New(strings.NewReader(""))
	_, err := p.NextEvent()
	if !errors.Is(err, io.EOF) {
		t.Errorf("empty stream: want EOF, got %v", err)
	}
}

func TestParseWhitespaceOnlyStream(t *testing.T) {
	p := New(strings.NewReader("   \n\n\n   \n\n"))
	_, err := p.NextEvent()
	if !errors.Is(err, io.EOF) {
		t.Errorf("whitespace-only stream: want EOF, got %v", err)
	}
}

func TestExtractDeltaTextEdgeCases(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		isErr bool
	}{
		// Standard delta
		{`{"choices":[{"delta":{"content":"hi"}}]}`, "hi", false},
		// Empty delta
		{`{"choices":[{"delta":{"content":""}}]}`, "", false},
		// Delta with role only (no content field)
		{`{"choices":[{"delta":{"role":"assistant"}}]}`, "", false},
		// Empty choices
		{`{"choices":[]}`, "", false},
		// No choices field
		{`{"id":"x"}`, "", false},
		// Malformed JSON — defensively returns empty string with no error.
		{`{not json`, "", false},
		// Null content
		{`{"choices":[{"delta":{"content":null}}]}`, "", false},
		// Numeric content (should still come through as string)
		{`{"choices":[{"delta":{"content":42}}]}`, "", false},
	}
	for _, c := range cases {
		got, err := ExtractDeltaText(c.in)
		if c.isErr && err == nil {
			t.Errorf("ExtractDeltaText(%q): expected error, got nil (got %q)", c.in, got)
		}
		if !c.isErr && err != nil {
			t.Errorf("ExtractDeltaText(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ExtractDeltaText(%q): want %q, got %q", c.in, c.want, got)
		}
	}
}

func TestExtractDoneMarker(t *testing.T) {
	// ExtractDeltaText on the [DONE] marker must not panic and must
	// return an empty string.
	got, err := ExtractDeltaText("[DONE]")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "" {
		t.Errorf("[DONE] marker should not extract text, got %q", got)
	}
}

func TestParseManyEventsSequential(t *testing.T) {
	// Build a stream with 100 events; verify all are read back in order.
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("data: event\n\n")
	}
	p := New(strings.NewReader(b.String()))
	count := 0
	for {
		_, err := p.NextEvent()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("err at event %d: %v", count, err)
		}
		count++
		if count > 200 {
			t.Fatal("runaway loop")
		}
	}
	if count != 100 {
		t.Errorf("want 100 events, got %d", count)
	}
}