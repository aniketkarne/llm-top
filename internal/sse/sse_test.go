package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseSimple(t *testing.T) {
	in := "data: hello\n\n"
	p := New(strings.NewReader(in))
	ev, err := p.NextEvent()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Data != "hello" {
		t.Fatalf("data: want hello, got %q", ev.Data)
	}
}

func TestParseMultiple(t *testing.T) {
	in := "data: a\n\ndata: b\n\n"
	p := New(strings.NewReader(in))
	evs, err := ParseAll(p.br)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(evs) != 2 || evs[0].Data != "a" || evs[1].Data != "b" {
		t.Fatalf("events wrong: %+v", evs)
	}
}

func TestParseDone(t *testing.T) {
	in := "data: {\"x\":1}\n\ndata: [DONE]\n\n"
	p := New(strings.NewReader(in))
	_, err := p.NextEvent()
	if err != nil {
		t.Fatalf("first err: %v", err)
	}
	_, err = p.NextEvent()
	if !errors.Is(err, ErrDone) {
		t.Fatalf("want ErrDone, got %v", err)
	}
}

func TestParseWithEventType(t *testing.T) {
	in := "event: ping\ndata: hi\n\n"
	p := New(strings.NewReader(in))
	ev, err := p.NextEvent()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Event != "ping" || ev.Data != "hi" {
		t.Fatalf("ev: %+v", ev)
	}
}

func TestParseIgnoresComments(t *testing.T) {
	in := ": hb\n\ndata: x\n\n"
	p := New(strings.NewReader(in))
	ev, err := p.NextEvent()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Data != "x" {
		t.Fatalf("data: want x, got %q", ev.Data)
	}
}

func TestEOFReturnsEOF(t *testing.T) {
	p := New(strings.NewReader(""))
	_, err := p.NextEvent()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestExtractDeltaText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"choices":[{"delta":{"content":"hello"}}]}`, "hello"},
		{`{"choices":[{"delta":{"content":"line1\nline2"}}]}`, "line1\nline2"},
		{`{"choices":[{"delta":{}}]}`, ""},
		{`plain text`, ""},
		{"", ""},
	}
	for _, c := range cases {
		got, err := ExtractDeltaText(c.in)
		if err != nil {
			t.Fatalf("err for %q: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ExtractDeltaText(%q): want %q, got %q", c.in, c.want, got)
		}
	}
}
