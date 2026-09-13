//go:build sqlite

package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// openTemp returns a Store backed by a per-test temp file. The file is
// cleaned up automatically by testing.T.
func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleRequest(id, model string) proxy.Request {
	return proxy.Request{
		ID:           id,
		StartedAt:    time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC),
		EndedAt:      time.Date(2026, 9, 13, 10, 0, 1, 500_000_000, time.UTC),
		Method:       "POST",
		Path:         "/v1/chat/completions",
		Model:        model,
		Upstream:     "https://api.openai.com",
		Provider:     "openai",
		Stream:       true,
		StatusCode:   200,
		TTFTMillis:   420,
		TotalMillis:  1500,
		PromptTokens: 100,
		OutputTokens: 50,
		CostUSD:      0.0006,
		PromptHash:   "deadbeef",
	}
}

func TestInsertAndGetRoundtrip(t *testing.T) {
	s := openTemp(t)
	r := sampleRequest("abc123", "gpt-5.5-mini")
	if err := s.Insert(r); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.Get("abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != r.ID || got.Model != r.Model || got.PromptHash != r.PromptHash {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, r)
	}
	if got.PromptTokens != r.PromptTokens || got.OutputTokens != r.OutputTokens {
		t.Errorf("tokens lost: %+v vs %+v", got, r)
	}
	if got.CostUSD != r.CostUSD {
		t.Errorf("cost lost: got %v, want %v", got.CostUSD, r.CostUSD)
	}
	if !got.Stream {
		t.Errorf("stream flag lost")
	}
	if got.StartedAt.IsZero() || got.EndedAt.IsZero() {
		t.Errorf("times lost: started=%v ended=%v", got.StartedAt, got.EndedAt)
	}
	if !got.StartedAt.Equal(r.StartedAt) {
		t.Errorf("started_at drift: %v vs %v", got.StartedAt, r.StartedAt)
	}
}

func TestGetNotFound(t *testing.T) {
	s := openTemp(t)
	_, err := s.Get("does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInsertDuplicateIsNoop(t *testing.T) {
	s := openTemp(t)
	r := sampleRequest("dup", "gpt-5.5")
	if err := s.Insert(r); err != nil {
		t.Fatal(err)
	}
	r.OutputTokens = 999 // mutate after first insert
	if err := s.Insert(r); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("dup")
	if got.OutputTokens != 50 {
		t.Errorf("duplicate insert should not overwrite: got %d, want 50", got.OutputTokens)
	}
}

func TestListFilters(t *testing.T) {
	s := openTemp(t)
	base := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	for i, m := range []string{"gpt-5.5", "gpt-5.5-mini", "claude-sonnet-5", "gpt-5.5-mini"} {
		r := sampleRequest(fmt.Sprintf("id%02d", i), m)
		r.StartedAt = base.Add(time.Duration(i) * time.Minute)
		r.Provider = "openai"
		if m == "claude-sonnet-5" {
			r.Provider = "anthropic"
		}
		if err := s.Insert(r); err != nil {
			t.Fatal(err)
		}
	}

	// Filter by model.
	got, err := s.List(ListFilter{Model: "gpt-5.5-mini"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("model filter: want 2, got %d", len(got))
	}

	// Filter by provider.
	got, err = s.List(ListFilter{Provider: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("provider filter: want 1, got %d", len(got))
	}

	// Filter by time window.
	got, err = s.List(ListFilter{Since: base.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("since filter: want 2, got %d", len(got))
	}
	got, err = s.List(ListFilter{
		Since: base.Add(1 * time.Minute),
		Until: base.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("since/until filter: want 2, got %d", len(got))
	}

	// Limit clamps the result.
	got, err = s.List(ListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("limit=2: got %d", len(got))
	}
}

func TestRecentOrder(t *testing.T) {
	s := openTemp(t)
	base := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		r := sampleRequest(string(rune('a'+i)), "gpt-5.5")
		r.StartedAt = base.Add(time.Duration(i) * time.Second)
		if err := s.Insert(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Recent(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Recent(3): want 3, got %d", len(got))
	}
	// Should be newest-first: e, d, c.
	if got[0].ID != "e" || got[1].ID != "d" || got[2].ID != "c" {
		t.Errorf("Recent ordering wrong: got %v %v %v",
			got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestRedactionIsCallersResponsibility(t *testing.T) {
	// Documented contract: the store does not redact. Inserting a
	// body with an unredacted key returns it verbatim. The caller
	// (the proxy) MUST redact before calling Insert.
	s := openTemp(t)
	r := sampleRequest("sec", "gpt-5.5")
	r.RequestBody = `{"model":"x","api_key":"sk-abc1234567890abcdef"}`
	if err := s.Insert(r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("sec")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.RequestBody, "sk-abc1234567890abcdef") {
		t.Errorf("store should preserve caller-provided body verbatim; got %q", got.RequestBody)
	}

	// And: when the caller has already redacted, the store preserves
	// that too — no double-replacement, no panic.
	r2 := sampleRequest("red", "gpt-5.5")
	r2.RequestBody = `{"model":"x","api_key":"sk-***"}`
	if err := s.Insert(r2); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Get("red")
	if !strings.Contains(got2.RequestBody, "sk-***") {
		t.Errorf("already-redacted body should round-trip: got %q", got2.RequestBody)
	}
}

func TestEmptyIDRejected(t *testing.T) {
	s := openTemp(t)
	if err := s.Insert(proxy.Request{}); err == nil {
		t.Fatal("Insert with empty ID should error")
	}
}

func TestLegacyEventsStillWork(t *testing.T) {
	s := openTemp(t)
	if err := s.Append(Event{
		Time:    time.Now(),
		Kind:    "request",
		Model:   "gpt-5.5",
		Content: "hello",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	j, err := s.DumpJSON()
	if err != nil {
		t.Fatalf("DumpJSON: %v", err)
	}
	if !strings.Contains(string(j), "hello") {
		t.Errorf("dump missing content: %s", j)
	}
}

func TestSessionIDFilter(t *testing.T) {
	s := openTemp(t)
	for i, sid := range []string{"s1", "s1", "s2", ""} {
		r := sampleRequest(string(rune('a'+i)), "gpt-5.5")
		r.SessionID = sid
		if err := s.Insert(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ListFilter{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("session filter: want 2, got %d", len(got))
	}
}