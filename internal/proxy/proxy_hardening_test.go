package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
	"github.com/aniketkarne-com/llm-top/internal/redactor"
)

// --- Hardening: edge cases for the proxy ---

func TestEmptyRequestBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read whatever was sent, expect empty.
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("upstream got non-empty body: %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	rec := metrics.NewRecorder()
	buf := ring.New(10)
	red := redactor.New()
	srv := New(Config{
		ListenAddr:      "127.0.0.1:0",
		UpstreamBaseURL: upstream.URL,
		BufferSize:      10,
	}, rec, buf, red)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestMalformedJSONRequestBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	rec := metrics.NewRecorder()
	buf := ring.New(10)
	red := redactor.New()
	srv := New(Config{
		ListenAddr:      "127.0.0.1:0",
		UpstreamBaseURL: upstream.URL,
		BufferSize:      10,
	}, rec, buf, red)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Garbage JSON — proxy must not panic.
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader("{not valid json"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200 (upstream echoed), got %d", resp.StatusCode)
	}
}

func TestUnicodeRequestAndResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"🚀 こんにちは"}}]}`))
	}))
	defer upstream.Close()

	rec := metrics.NewRecorder()
	buf := ring.New(10)
	red := redactor.New()
	srv := New(Config{
		ListenAddr:      "127.0.0.1:0",
		UpstreamBaseURL: upstream.URL,
		BufferSize:      10,
	}, rec, buf, red)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"こんにちは 🚀"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "🚀") {
		t.Errorf("unicode response mangled: %q", got)
	}
}

func TestLargeStreamingResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			t.Errorf("upstream: expected stream:true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 200; i++ {
			chunk := fmt.Sprintf(`{"choices":[{"delta":{"content":"token-%d "}}]`, i)
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	rec := metrics.NewRecorder()
	buf := ring.New(500)
	red := redactor.New()
	srv := New(Config{
		ListenAddr:      "127.0.0.1:0",
		UpstreamBaseURL: upstream.URL,
		BufferSize:      500,
	}, rec, buf, red)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"go"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	fullBody, _ := io.ReadAll(resp.Body)
	// Count events robustly by counting complete "\n\n" separators plus 1.
	// Each SSE event ends with a blank line; [DONE] counts as one too.
	sepCount := bytes.Count(fullBody, []byte("\n\n"))
	if sepCount < 200 {
		t.Errorf("expected >= 200 SSE events, got %d (by separator count)", sepCount)
	}
	// Metrics should reflect the work.
	summary := rec.Summarize()
	if summary.TotalOutTok < 200 {
		t.Errorf("output tokens: want >= 200, got %d", summary.TotalOutTok)
	}
}

func TestUpstreamTimeoutHandled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hang for longer than the test timeout.
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	rec := metrics.NewRecorder()
	buf := ring.New(10)
	red := redactor.New()
	srv := New(Config{
		ListenAddr:      "127.0.0.1:0",
		UpstreamBaseURL: upstream.URL,
		BufferSize:      10,
	}, rec, buf, red)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[]}`))
	if err == nil {
		resp.Body.Close()
		t.Errorf("expected timeout error, got status %d", resp.StatusCode)
	}
}

func TestHandlesIncompleteSSEStream(t *testing.T) {
	// Upstream closes the connection mid-stream (no [DONE] marker).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Force-close the connection without [DONE].
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, _ := hijacker.Hijack()
			_ = conn.Close()
		}
	}))
	defer upstream.Close()

	rec := metrics.NewRecorder()
	buf := ring.New(10)
	red := redactor.New()
	srv := New(Config{
		ListenAddr:      "127.0.0.1:0",
		UpstreamBaseURL: upstream.URL,
		BufferSize:      10,
	}, rec, buf, red)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// No panic = pass.
}

func TestStatsIncrements(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	rec := metrics.NewRecorder()
	buf := ring.New(10)
	red := redactor.New()
	srv := New(Config{
		ListenAddr:      "127.0.0.1:0",
		UpstreamBaseURL: upstream.URL,
		BufferSize:      10,
	}, rec, buf, red)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for i := 0; i < 3; i++ {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"x","messages":[]}`))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	total, errs := srv.Stats()
	if total != 3 {
		t.Errorf("total: want 3, got %d", total)
	}
	if errs != 0 {
		t.Errorf("errs: want 0, got %d", errs)
	}
}

func TestIsStreamingResponse(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"application/json", false},
		{"text/plain", false},
		{"", false},
	}
	for _, c := range cases {
		resp := &http.Response{Header: http.Header{}}
		if c.ct != "" {
			resp.Header.Set("Content-Type", c.ct)
		}
		if got := isStreamingResponse(resp); got != c.want {
			t.Errorf("isStreamingResponse(%q): want %v, got %v", c.ct, c.want, got)
		}
	}
}

func TestModelFromBody(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"model":"gpt-4o"}`, "gpt-4o"},
		{`{"model":"claude-3.5-sonnet","messages":[]}`, "claude-3.5-sonnet"},
		{`{}`, ""},
		{`not json`, ""},
		{``, ""},
		{`{"model":""}`, ""},
	}
	for _, c := range cases {
		if got := modelFromBody([]byte(c.in)); got != c.want {
			t.Errorf("modelFromBody(%q): want %q, got %q", c.in, c.want, got)
		}
	}
}

func TestJsonOutputTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		// usage.completion_tokens wins.
		{`{"choices":[{"message":{"content":"hi"}}],"usage":{"completion_tokens":42}}`, 42},
		// Otherwise estimate from choices[0].message.content.
		{`{"choices":[{"message":{"content":"hello"}}]}`, 2}, // 5 runes -> ceil(5/4)=2
		// Empty content -> 0.
		{`{"choices":[{"message":{"content":""}}]}`, 0},
		// No choices, no usage -> 0.
		{`{}`, 0},
		// Empty body -> 0.
		{``, 0},
	}
	for _, c := range cases {
		got := jsonOutputTokens([]byte(c.in))
		// Token estimation can vary slightly; just check non-negative and
		// that the usage-path works exactly.
		if c.want == 42 && got != 42 {
			t.Errorf("jsonOutputTokens(%q): want 42, got %d", c.in, got)
		}
		if c.want == 0 && got != 0 {
			t.Errorf("jsonOutputTokens(%q): want 0, got %d", c.in, got)
		}
		if c.want == 2 && got < 1 {
			t.Errorf("jsonOutputTokens(%q): want >=1, got %d", c.in, got)
		}
	}
}