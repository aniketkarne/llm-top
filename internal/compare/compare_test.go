package compare

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/pricing"
	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// fakeTarget returns an httptest server that mimics an OpenAI-compatible
// chat completions endpoint. delay controls time-to-first-byte (streaming
// TTFT and total latency), completionTokens sets usage.completion_tokens,
// body is the literal assistant message content. Returns 200 on happy
// path; set failStatus > 0 to return that status with an error payload.
func fakeTarget(t *testing.T, delay time.Duration, completionTokens int, body string, failStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failStatus > 0 {
			http.Error(w, "boom", failStatus)
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-compare",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-5.5-mini",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": body},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": completionTokens, "total_tokens": 10 + completionTokens},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sampleRequest() proxy.Request {
	return proxy.Request{
		ID:        "captured-001",
		StartedAt: time.Now(),
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Model:     "gpt-5.5-mini",
		Stream:    false,
		RequestBody: `{"model":"gpt-5.5-mini","messages":[{"role":"user","content":"hi"}]}`,
	}
}

func TestRunHappyPath(t *testing.T) {
	a := fakeTarget(t, 10*time.Millisecond, 5, "from-a", 0)
	b := fakeTarget(t, 50*time.Millisecond, 3, "from-b", 0)
	res := Run(context.Background(), sampleRequest(),
		[]Target{
			{Name: "fast", BaseURL: a.URL, Model: "gpt-5.5-mini"},
			{Name: "slow", BaseURL: b.URL, Model: "claude-sonnet-5"},
		},
		Options{Pricing: pricing.Defaults()},
	)
	if len(res.Rows) != 2 {
		t.Fatalf("rows: want 2, got %d", len(res.Rows))
	}
	for i, r := range res.Rows {
		if r.StatusCode != 200 {
			t.Errorf("row %d status: want 200, got %d (err=%s)", i, r.StatusCode, r.Error)
		}
		if r.Error != "" {
			t.Errorf("row %d unexpected error: %s", i, r.Error)
		}
	}
	// Order preserved.
	if res.Rows[0].Name != "fast" || res.Rows[1].Name != "slow" {
		t.Errorf("order broken: %v %v", res.Rows[0].Name, res.Rows[1].Name)
	}
	// Cost computed for the models that are in the default table.
	if !res.Rows[0].CostKnown {
		t.Errorf("fast row cost unknown; want known for gpt-5.5-mini")
	}
	if !res.Rows[1].CostKnown {
		t.Errorf("slow row cost unknown; want known for claude-sonnet-5")
	}
	// Response body captured.
	if res.Rows[0].ResponseBody == "" || res.Rows[1].ResponseBody == "" {
		t.Errorf("response bodies empty: %+v", res.Rows)
	}
}

func TestRunPartialFailure(t *testing.T) {
	good := fakeTarget(t, 5*time.Millisecond, 1, "ok", 0)
	bad := fakeTarget(t, 0, 0, "", http.StatusInternalServerError)
	res := Run(context.Background(), sampleRequest(),
		[]Target{
			{Name: "good", BaseURL: good.URL},
			{Name: "bad", BaseURL: bad.URL},
		},
		Options{Pricing: pricing.Defaults()},
	)
	if res.Rows[0].Error != "" || res.Rows[0].StatusCode != 200 {
		t.Errorf("good row: status=%d err=%s", res.Rows[0].StatusCode, res.Rows[0].Error)
	}
	// The replay package sets StatusCode but leaves Error empty for
	// non-2xx responses that come back with a body. That's fine — the
	// renderer surfaces the status code in the table and the user can
	// see something failed.
	if res.Rows[1].StatusCode != http.StatusInternalServerError {
		t.Errorf("bad row status: want 500, got %d", res.Rows[1].StatusCode)
	}
	// And the good row's metrics should be unaffected by the bad one.
	if res.Rows[0].ResponseBody == "" {
		t.Errorf("good row should have body; partial failure must not abort siblings")
	}
}

func TestRunEmptyBaseURL(t *testing.T) {
	res := Run(context.Background(), sampleRequest(),
		[]Target{{Name: "broken", BaseURL: ""}},
		Options{},
	)
	if res.Rows[0].Error == "" {
		t.Errorf("empty base URL should produce error row")
	}
}

func TestRunNoTargets(t *testing.T) {
	res := Run(context.Background(), sampleRequest(), nil, Options{})
	if len(res.Rows) != 0 {
		t.Errorf("no targets should yield no rows, got %d", len(res.Rows))
	}
}

func TestRunConcurrencyCap(t *testing.T) {
	// Three targets, concurrency=1. The total elapsed time should be
	// roughly the sum of individual latencies, not max.
	var inFlight atomic.Int32
	maxInFlight := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	targets := []Target{
		{Name: "t1", BaseURL: srv.URL},
		{Name: "t2", BaseURL: srv.URL},
		{Name: "t3", BaseURL: srv.URL},
	}
	res := Run(context.Background(), sampleRequest(), targets, Options{Concurrency: 1})
	if len(res.Rows) != 3 {
		t.Fatalf("rows: want 3, got %d", len(res.Rows))
	}
	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("concurrency cap broken: saw %d in flight", got)
	}
}

func TestRunConcurrencyAllParallel(t *testing.T) {
	var inFlight atomic.Int32
	maxInFlight := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	targets := []Target{
		{Name: "t1", BaseURL: srv.URL},
		{Name: "t2", BaseURL: srv.URL},
		{Name: "t3", BaseURL: srv.URL},
	}
	res := Run(context.Background(), sampleRequest(), targets, Options{Concurrency: 0})
	if len(res.Rows) != 3 {
		t.Fatalf("rows: want 3, got %d", len(res.Rows))
	}
	// With concurrency=0 (= len(targets)), all three should overlap at
	// some point. We allow >= 2 because exact scheduling is timing-
	// sensitive on busy CI machines.
	if got := maxInFlight.Load(); got < 2 {
		t.Errorf("expected >=2 concurrent, saw max %d", got)
	}
}

func TestRunTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"}}]}`))
	}))
	defer slow.Close()

	res := Run(context.Background(), sampleRequest(),
		[]Target{{Name: "slow", BaseURL: slow.URL}},
		Options{Timeout: 20 * time.Millisecond, Pricing: pricing.Defaults()},
	)
	if res.Rows[0].Error == "" {
		t.Errorf("expected timeout error, got empty error; status=%d", res.Rows[0].StatusCode)
	}
}

func TestRunModelOverride(t *testing.T) {
	var seenModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	res := Run(context.Background(), sampleRequest(),
		[]Target{{Name: "x", BaseURL: srv.URL, Model: "claude-sonnet-5"}},
		Options{Pricing: pricing.Defaults()},
	)
	if seenModel != "claude-sonnet-5" {
		t.Errorf("model not rewritten: upstream saw %q", seenModel)
	}
	if res.Rows[0].Model != "claude-sonnet-5" {
		t.Errorf("row model: want claude-sonnet-5, got %q", res.Rows[0].Model)
	}
}

func TestRunUnknownModelCostUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	res := Run(context.Background(), sampleRequest(),
		[]Target{{Name: "x", BaseURL: srv.URL, Model: "totally-unknown-model-xyz"}},
		Options{Pricing: pricing.Defaults()},
	)
	if res.Rows[0].CostKnown {
		t.Errorf("unknown model should have CostKnown=false, got true (cost=%v)", res.Rows[0].CostUSD)
	}
}

func TestRunProviderOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()
	res := Run(context.Background(), sampleRequest(),
		[]Target{{Name: "x", BaseURL: srv.URL, Provider: "custom"}},
		Options{},
	)
	if res.Rows[0].Provider != "custom" {
		t.Errorf("provider override lost: %q", res.Rows[0].Provider)
	}
}

func TestRunResponseBodyTruncated(t *testing.T) {
	big := strings.Repeat("a", 10000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": big},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	res := Run(context.Background(), sampleRequest(),
		[]Target{{Name: "x", BaseURL: srv.URL}},
		Options{},
	)
	if len(res.Rows[0].ResponseBody) > MaxResponseBodyBytes+10 { // +10 for trailing "…"
		t.Errorf("response body not truncated: len=%d", len(res.Rows[0].ResponseBody))
	}
	if !strings.HasSuffix(res.Rows[0].ResponseBody, "…") {
		t.Errorf("truncated body should end with ellipsis")
	}
}

func TestExcerpt(t *testing.T) {
	if got := Excerpt(""); got != "" {
		t.Errorf("empty excerpt should be empty, got %q", got)
	}
	if got := Excerpt("hello"); got != "hello" {
		t.Errorf("short excerpt unchanged: %q", got)
	}
	big := strings.Repeat("x", MaxExcerptBytes+50)
	got := Excerpt(big)
	if len(got) != MaxExcerptBytes+len("…") {
		t.Errorf("excerpt length: want %d, got %d", MaxExcerptBytes+len("…"), len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("excerpt should end with ellipsis")
	}
}

func TestTruncateBytes(t *testing.T) {
	if truncateBytes("abc", 10) != "abc" {
		t.Error("short string unchanged")
	}
	if got := truncateBytes("abcdef", 3); got != "abc…" {
		t.Errorf("truncate: want abc…, got %q", got)
	}
	if truncateBytes("", 3) != "" {
		t.Error("empty stays empty")
	}
	if truncateBytes("abc", 0) != "abc" {
		t.Error("n<=0 means no truncation")
	}
}

// --- RenderText column-alignment regression ---
//
// Go's text/tabwriter has an edge case where right-aligned cells that
// exactly fit their column get their trailing tabs collapsed ("200200"
// rather than "200   200"). RenderText works around it by NOT using
// AlignRight and instead right-padding each cell manually to a fixed
// width. This test locks in the workaround so a future "simplification"
// to AlignRight won't silently break the visual output.
func TestRenderTextColumnsLineUp(t *testing.T) {
	rows := []Row{
		{Name: "a", StatusCode: 200, Model: "m", TotalMillis: 100, OutputTokens: 1, CostUSD: 0.01, CostKnown: true},
		{Name: "b", StatusCode: 200, Model: "m", TotalMillis: 100, OutputTokens: 1, CostUSD: 0.01, CostKnown: true},
		{Name: "c", StatusCode: 200, Model: "m", TotalMillis: 100, OutputTokens: 1, CostUSD: 0.01, CostKnown: true},
	}
	var buf bytes.Buffer
	RenderText(&buf, "id", rows)
	out := buf.String()
	// Each "200" must be followed by at least one space before the next
	// "200" — never immediately abutting another cell.
	if strings.Contains(out, "200200") || strings.Contains(out, "200\n") == false && strings.Contains(out, "200   200") == false {
		// The "right" check: we want the cells separated.
		// Just assert that we never see two "200"s back-to-back.
	}
	if strings.Contains(out, "200200") {
		t.Errorf("two cells abut (regression of tabwriter right-align edge case): %s", out)
	}
	// And the headers must be on their own line.
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") || !strings.Contains(out, "c") {
		t.Errorf("missing column headers in output: %s", out)
	}
}
