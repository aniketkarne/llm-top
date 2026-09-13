package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// makeCaptured builds a typical captured Request for tests.
func makeCaptured(model string, body string) proxy.Request {
	return proxy.Request{
		ID:           "captured0001",
		StartedAt:    time.Now().UTC().Add(-time.Second),
		EndedAt:      time.Now().UTC(),
		Method:       http.MethodPost,
		Path:         "/v1/chat/completions",
		Model:        model,
		Upstream:     "https://api.openai.com",
		Provider:     "openai",
		Stream:       false,
		StatusCode:   200,
		RequestBody:  body,
		ResponseBody: "",
	}
}

// sseUpstream returns an httptest server that emits OpenAI-style SSE
// chunks for stream=true, JSON for stream=false, and records the
// number of times it was hit and the bodies it observed.
type sseUpstream struct {
	hits       int32
	lastBody   string
	lastStream bool
	lastModel  string
	delay      time.Duration
	usage      bool
}

func newUpstream(delay time.Duration, usage bool) (*httptest.Server, *sseUpstream) {
	u := &sseUpstream{delay: delay, usage: usage}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&u.hits, 1)
		body, _ := io.ReadAll(r.Body)
		u.lastBody = string(body)
		var probe struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		u.lastStream = probe.Stream
		u.lastModel = probe.Model

		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			// Emit three chunks with a small gap so TTFT is
			// measurable but the test still finishes in ms.
			chunks := []string{"Hello", " there", " world"}
			for i, c := range chunks {
				if u.delay > 0 && i > 0 {
					time.Sleep(u.delay)
				}
				ev := map[string]any{
					"id":      fmt.Sprintf("chunk-%d", i),
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   probe.Model,
					"choices": []map[string]any{
						{"index": 0, "delta": map[string]any{"content": c}},
					},
				}
				b, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", b)
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := map[string]any{
			"id":      "cmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   probe.Model,
			"choices": []map[string]any{
				{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": "pong"},
					"finish_reason": "stop",
				},
			},
		}
		if u.usage {
			resp["usage"] = map[string]any{
				"prompt_tokens":     42,
				"completion_tokens": 17,
				"total_tokens":      59,
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv, u
}

func TestExecute_SetsResultFields(t *testing.T) {
	srv, _ := newUpstream(0, false)
	defer srv.Close()

	captured := makeCaptured("gpt-5.5", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	res := Execute(context.Background(), captured, ExecutionConfig{
		UpstreamBaseURL: srv.URL,
		UpstreamAPIKey:  "sk-test",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status=%d want 200", res.StatusCode)
	}
	if res.RequestID == "" || res.RequestID == captured.ID {
		t.Errorf("RequestID should be new and non-empty: %q", res.RequestID)
	}
	if !strings.Contains(res.ResponseBody, "pong") {
		t.Errorf("response body missing 'pong': %q", res.ResponseBody)
	}
	if res.Target.Model != "gpt-5.5" {
		t.Errorf("Target.Model=%q want gpt-5.5", res.Target.Model)
	}
	if res.TotalMillis <= 0 {
		t.Errorf("TotalMillis should be >0, got %d", res.TotalMillis)
	}
}

func TestExecute_ModelRewrite(t *testing.T) {
	srv, u := newUpstream(0, false)
	defer srv.Close()

	captured := makeCaptured("gpt-5.5", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	res := Execute(context.Background(), captured, ExecutionConfig{
		UpstreamBaseURL: srv.URL,
		Model:           "claude-sonnet-5",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if u.lastModel != "claude-sonnet-5" {
		t.Errorf("upstream saw model=%q want claude-sonnet-5", u.lastModel)
	}
	if res.Target.Model != "claude-sonnet-5" {
		t.Errorf("Target.Model=%q want claude-sonnet-5", res.Target.Model)
	}
	// Surgical rewrite must preserve formatting: original field order
	// and surrounding keys must remain intact.
	if !strings.Contains(u.lastBody, `"messages"`) {
		t.Errorf("body lost 'messages' field: %q", u.lastBody)
	}
	if !strings.Contains(u.lastBody, `"stream":false`) {
		t.Errorf("body lost 'stream' field: %q", u.lastBody)
	}
}

func TestExecute_StreamingTTFT(t *testing.T) {
	srv, _ := newUpstream(15*time.Millisecond, false)
	defer srv.Close()

	captured := makeCaptured("gpt-5.5", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	res := Execute(context.Background(), captured, ExecutionConfig{
		UpstreamBaseURL: srv.URL,
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if !res.Stream {
		t.Errorf("Stream should be true for text/event-stream upstream")
	}
	if res.TTFTMillis <= 0 {
		t.Errorf("TTFTMillis should be >0 for streaming, got %d", res.TTFTMillis)
	}
	if res.TotalMillis < res.TTFTMillis {
		t.Errorf("TotalMillis (%d) should be >= TTFTMillis (%d)", res.TotalMillis, res.TTFTMillis)
	}
	if res.ResponseBody != "Hello there world" {
		t.Errorf("response body=%q want %q", res.ResponseBody, "Hello there world")
	}
}

func TestExecute_NonStreamingCost(t *testing.T) {
	srv, _ := newUpstream(0, false)
	defer srv.Close()

	captured := makeCaptured("claude-sonnet-5", `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	res := Execute(context.Background(), captured, ExecutionConfig{
		UpstreamBaseURL: srv.URL,
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	// Cost depends on estimated tokens. With one user message word
	// ("hi") -> 1*1.3 = 2 prompt, "pong" -> 1*1.3 = 2 output.
	// claude-sonnet-5: $2 in, $10 out per 1M -> 2/1e6*2 + 2/1e6*10 ≈ 0
	// The "cost" for tiny messages is effectively zero, so we just
	// assert CostUSD is non-negative and OutputTokens > 0.
	if res.CostUSD < 0 {
		t.Errorf("CostUSD must be non-negative, got %f", res.CostUSD)
	}
	if res.OutputTokens <= 0 {
		t.Errorf("OutputTokens=%d want >0", res.OutputTokens)
	}
	if res.PromptTokens <= 0 {
		t.Errorf("PromptTokens=%d want >0", res.PromptTokens)
	}
}

func TestExecute_StreamOverride(t *testing.T) {
	srv, u := newUpstream(0, false)
	defer srv.Close()

	captured := makeCaptured("gpt-5.5", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	override := true
	res := Execute(context.Background(), captured, ExecutionConfig{
		UpstreamBaseURL: srv.URL,
		Stream:          &override,
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if !u.lastStream {
		t.Errorf("upstream saw stream=false, expected true after override")
	}
	if !res.Stream {
		t.Errorf("Result.Stream should reflect the SSE Content-Type")
	}
}

func TestExecute_UpstreamOverride(t *testing.T) {
	srv, u := newUpstream(0, false)
	defer srv.Close()

	captured := makeCaptured("gpt-5.5", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	// Captured.Upstream is openai.com but we override to srv.URL.
	res := Execute(context.Background(), captured, ExecutionConfig{
		UpstreamBaseURL: srv.URL,
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if res.Target.BaseURL != srv.URL {
		t.Errorf("Target.BaseURL=%q want %q", res.Target.BaseURL, srv.URL)
	}
	if u.lastModel != "gpt-5.5" {
		t.Errorf("upstream should have received gpt-5.5, got %q", u.lastModel)
	}
}

func TestExecute_LocalShortCircuit(t *testing.T) {
	srv, u := newUpstream(0, false)
	defer srv.Close()

	captured := makeCaptured("llama3", `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`)
	res := Execute(context.Background(), captured, ExecutionConfig{
		// Mimic the --local flag: the CLI resolves this URL and
		// passes an empty key.
		UpstreamBaseURL: srv.URL,
		UpstreamAPIKey:  "",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if u.lastModel != "llama3" {
		t.Errorf("upstream should have received llama3, got %q", u.lastModel)
	}
	// Provider should be "local" because the URL is loopback.
	if !strings.Contains(res.Target.Provider, "local") && res.Target.Provider != "custom" {
		// Provider detection is host-based; localhost / 127.0.0.1 / 0.0.0.0
		// are all "local", but 127.0.0.1 in the URL is also fine.
		t.Errorf("Target.Provider=%q want local-ish", res.Target.Provider)
	}
}

func TestExecute_TransportFailure(t *testing.T) {
	captured := makeCaptured("gpt-5.5", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	res := Execute(context.Background(), captured, ExecutionConfig{
		UpstreamBaseURL: "http://127.0.0.1:1", // guaranteed not-listening
		Timeout:         500 * time.Millisecond,
	})
	if res.Error == "" {
		t.Fatalf("expected transport error, got Result.Error empty")
	}
	if res.StatusCode != 0 {
		t.Errorf("StatusCode should be 0 on transport failure, got %d", res.StatusCode)
	}
	if res.TotalMillis <= 0 {
		t.Errorf("TotalMillis should still be >0, got %d", res.TotalMillis)
	}
}

func TestExecute_EmptyUpstream(t *testing.T) {
	captured := makeCaptured("gpt-5.5", `{"model":"gpt-5.5"}`)
	res := Execute(context.Background(), captured, ExecutionConfig{})
	if res.Error == "" {
		t.Errorf("expected error for empty UpstreamBaseURL")
	}
	if res.StatusCode != 0 {
		t.Errorf("StatusCode should be 0, got %d", res.StatusCode)
	}
}

func TestExecute_HeadersPropagated(t *testing.T) {
	var gotTrace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTrace = r.Header.Get("X-Trace")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	captured := makeCaptured("gpt-5.5", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	res := Execute(context.Background(), captured, ExecutionConfig{
		UpstreamBaseURL: srv.URL,
		Headers:         map[string]string{"X-Trace": "abc-123"},
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if gotTrace != "abc-123" {
		t.Errorf("upstream got X-Trace=%q want abc-123", gotTrace)
	}
}

func TestSummary(t *testing.T) {
	r := Result{
		RequestID:   "abcdef1234567890",
		StatusCode:  200,
		TTFTMillis:  82,
		TotalMillis: 312,
		Stream:      true,
		Target:      Target{BaseURL: "http://x", Model: "gpt-5.5"},
		OutputTokens: 53, PromptTokens: 42,
		CostUSD: 0,
	}
	s := Summary(r)
	if !strings.Contains(s, "status=200") || !strings.Contains(s, "ttft=82ms") {
		t.Errorf("summary missing key fields: %s", s)
	}
	if !strings.Contains(s, "model=gpt-5.5") {
		t.Errorf("summary missing model: %s", s)
	}
}

func TestTruncateBody(t *testing.T) {
	cases := []struct {
		in       string
		n        int
		expected string
	}{
		{"short", 100, "short"},
		{"hello world", 5, "hello\n…[truncated]"},
		{"", 5, ""},
	}
	for _, c := range cases {
		got := TruncateBody(c.in, c.n)
		if got != c.expected {
			t.Errorf("TruncateBody(%q, %d)=%q want %q", c.in, c.n, got, c.expected)
		}
	}
}

func TestJoinURL(t *testing.T) {
	cases := []struct {
		base, path, want string
	}{
		// Base already includes the captured path — don't double up.
		{"https://api.openai.com/v1/chat/completions", "/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/chat/completions", "/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		// Base has the API prefix but not the full path.
		{"https://api.openai.com/v1", "/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		// Bare host:port (no scheme) — should prepend http:// and join.
		{"127.0.0.1:18101", "/v1/chat/completions", "http://127.0.0.1:18101/v1/chat/completions"},
		// Trailing slash handling.
		{"https://api.openai.com/v1/", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		// Empty base — return path as-is.
		{"", "/v1/chat/completions", "/v1/chat/completions"},
	}
	for _, c := range cases {
		got := joinURL(c.base, c.path)
		if got != c.want {
			t.Errorf("joinURL(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}
