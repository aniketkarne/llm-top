package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewIDUniqueness(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewID()
		if len(id) != 16 {
			t.Fatalf("id length: want 16, got %d (%q)", len(id), id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id after %d iterations: %q", i, id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("unique ids: want %d, got %d", n, len(seen))
	}
}

func TestHashPromptStability(t *testing.T) {
	a := []byte(`{"model":"x","messages":[{"role":"user","content":"hello world"}]}`)
	b := []byte(`{"model":"x","messages":[{"role":"user","content":"hello    world"}]}`)
	c := []byte(`{"model":"x","messages":[{"role":"user","content":"Hello world"}]}`)
	d := []byte(`{"model":"x","messages":[{"role":"system","content":"ignore"},{"role":"user","content":"hi"}]}`)

	ha := HashPrompt(a)
	hb := HashPrompt(b)
	if ha == "" {
		t.Fatal("expected non-empty hash for a")
	}
	if ha != hb {
		t.Errorf("whitespace should normalize: %s != %s", ha, hb)
	}
	if ha == HashPrompt(c) {
		t.Errorf("case-sensitive: hash should differ between 'Hello' and 'hello'")
	}
	hd := HashPrompt(d)
	if hd == "" {
		t.Fatal("expected non-empty hash for user message present")
	}
	if ha == hd {
		t.Errorf("different prompts should not collide: %s == %s", ha, hd)
	}
}

func TestHashPromptEmptyAndInvalid(t *testing.T) {
	if h := HashPrompt(nil); h != "" {
		t.Errorf("nil body should hash to empty, got %q", h)
	}
	if h := HashPrompt([]byte("")); h != "" {
		t.Errorf("empty body should hash to empty, got %q", h)
	}
	if h := HashPrompt([]byte("not json")); h != "" {
		t.Errorf("non-json body should hash to empty, got %q", h)
	}
	if h := HashPrompt([]byte(`{"messages":[]}`)); h != "" {
		t.Errorf("empty messages should hash to empty, got %q", h)
	}
	// System-only messages produce no user content -> empty hash.
	if h := HashPrompt([]byte(`{"messages":[{"role":"system","content":"x"}]}`)); h != "" {
		t.Errorf("system-only should hash to empty, got %q", h)
	}
}

func TestProviderFromUpstream(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"https://api.openai.com", "openai"},
		{"https://api.openai.com/v1", "openai"},
		{"https://api.anthropic.com", "anthropic"},
		{"http://localhost:11434/v1", "local"},
		{"http://127.0.0.1:11434/v1", "local"},
		{"http://0.0.0.0:8080/v1", "local"},
		{"https://my-proxy.example.com", "custom"},
		{"", ""},
	}
	for _, c := range cases {
		got := ProviderFromUpstream(c.base)
		if got != c.want {
			t.Errorf("ProviderFromUpstream(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestRequestJSONRoundTrip(t *testing.T) {
	r := Request{
		ID:            "abc123",
		StartedAt:     time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC),
		EndedAt:       time.Date(2026, 9, 13, 10, 0, 1, 0, time.UTC),
		Method:        "POST",
		Path:          "/v1/chat/completions",
		Model:         "gpt-5.5-mini",
		Upstream:      "https://api.openai.com",
		Provider:      "openai",
		Stream:        true,
		StatusCode:    200,
		TTFTMillis:    420,
		TotalMillis:   1200,
		PromptTokens:  100,
		OutputTokens:  50,
		CostUSD:       0.0001,
		RequestBody:   `{"model":"gpt-5.5-mini"}`,
		ResponseBody:  "hello",
		PromptHash:    "deadbeef",
		SessionID:     "sess-1",
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(r.StartedAt) || !got.EndedAt.Equal(r.EndedAt) {
		t.Errorf("time round-trip failed: %+v", got)
	}
	if got.ID != r.ID || got.Model != r.Model || got.PromptHash != r.PromptHash || got.SessionID != r.SessionID {
		t.Errorf("field round-trip failed: %+v", got)
	}
}

func TestRequestTimeNormalization(t *testing.T) {
	// Times should round-trip in UTC even if the caller handed us local TZ.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata available; skipping normalization test")
	}
	local := time.Date(2026, 9, 13, 6, 0, 0, 0, loc) // 10:00 UTC
	utc := local.UTC()
	r := Request{StartedAt: local, EndedAt: utc}
	if !r.StartedAt.Equal(utc) {
		t.Errorf("local time not normalized: got %v, want %v", r.StartedAt, utc)
	}
	if !r.EndedAt.Equal(utc) {
		t.Errorf("utc time mutated: got %v", r.EndedAt)
	}
}

func TestIDIsHexAndReasonablyRandom(t *testing.T) {
	id := NewID()
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("id contains non-hex char %q in %q", r, id)
		}
	}
	if bytes.Equal([]byte(id[:8]), []byte(id[8:])) {
		t.Errorf("two halves of id should differ: %s", id)
	}
}