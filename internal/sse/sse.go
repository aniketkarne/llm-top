// Package sse parses Server-Sent Events (text/event-stream) streams as
// produced by OpenAI-compatible chat completions endpoints.
//
// The parser is deliberately minimal: it reads a bufio.Scanner line by line,
// recognizes "data: <json>" lines, and accumulates them into Events with a
// parsed Data payload. Special tokens "[DONE]" terminate the stream cleanly.
package sse

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Event is a single SSE event with the joined data payload.
type Event struct {
	Event string // event name (defaults to "message" if absent)
	Data  string // payload (may contain multiple newlines joined with "\n")
}

// ErrDone indicates the upstream signaled stream completion via "data: [DONE]".
var ErrDone = errors.New("sse: stream complete")

// Parser reads SSE frames from an io.Reader. Create with New.
type Parser struct {
	br *bufio.Reader
}

// New returns a Parser wrapping r.
func New(r io.Reader) *Parser {
	return &Parser{br: bufio.NewReaderSize(r, 64*1024)}
}

// NextEvent returns the next SSE event. Returns io.EOF at stream end,
// ErrDone on "[DONE]" sentinel, or another error on malformed input.
func (p *Parser) NextEvent() (Event, error) {
	var (
		eventType string
		dataLines []string
	)
	for {
		line, err := p.br.ReadString('\n')
		if err != nil {
			if len(dataLines) == 0 && eventType == "" {
				return Event{}, err
			}
			// flush partial event on error
			return Event{Event: defaultEvent(eventType), Data: strings.Join(dataLines, "\n")}, err
		}
		// strip trailing CR/LF
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// dispatch on blank line
			if eventType != "" || len(dataLines) > 0 {
				return Event{
					Event: defaultEvent(eventType),
					Data:  strings.Join(dataLines, "\n"),
				}, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			// comment / heartbeat
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			if d == "[DONE]" {
				return Event{}, ErrDone
			}
			dataLines = append(dataLines, d)
			continue
		}
		// unknown field — ignore per SSE spec
	}
}

func defaultEvent(t string) string {
	if t == "" {
		return "message"
	}
	return t
}

// ParseAll reads the full stream and returns all events, stopping on "[DONE]"
// or EOF. Useful for tests; not for production use (prefer streaming).
func ParseAll(r io.Reader) ([]Event, error) {
	p := New(r)
	var out []Event
	for {
		ev, err := p.NextEvent()
		if err != nil {
			if errors.Is(err, ErrDone) || errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, ev)
	}
}

// ExtractDeltaText scans an SSE event payload and returns the textual delta
// according to OpenAI's chat.completion.chunk schema. If the payload is not
// parseable JSON, it returns "" and a nil error so streaming can continue.
func ExtractDeltaText(payload string) (string, error) {
	if payload == "" {
		return "", nil
	}
	// Cheap detection: must look like JSON object.
	if !strings.HasPrefix(strings.TrimSpace(payload), "{") {
		return "", nil
	}
	// Locate "delta":{"content":"..."}. We use a small substring scan rather
	// than a full JSON parser to keep allocations low and parsing fast.
	const key = `"content":"`
	idx := strings.Index(payload, key)
	if idx < 0 {
		return "", nil
	}
	rest := payload[idx+len(key):]
	// Read until unescaped closing quote.
	var b strings.Builder
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c == '\\' && i+1 < len(rest) {
			next := rest[i+1]
			switch next {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'u':
				// skip 4 hex digits conservatively
				if i+5 < len(rest) {
					b.WriteString(rest[i+2 : i+6])
				}
				i += 4
			default:
				b.WriteByte(next)
			}
			i++
			continue
		}
		if c == '"' {
			break
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

// String renders an event for human logging (safe to print).
func (e Event) String() string {
	return fmt.Sprintf("event=%s data=%q", e.Event, e.Data)
}
