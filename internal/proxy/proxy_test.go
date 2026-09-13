package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
	"github.com/aniketkarne-com/llm-top/internal/redactor"
)

// startFakeUpstream returns an httptest.Server that mimics an OpenAI-compatible
// chat completions endpoint for both streaming and non-streaming responses.
func startFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("expected rewritten Authorization, got %q", got)
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			fl, _ := w.(http.Flusher)
			chunks := []string{
				`{"choices":[{"delta":{"content":"hello "}}]}`,
				`{"choices":[{"delta":{"content":"world"}}]}`,
				`{"choices":[{"delta":{}}]}`,
				`[DONE]`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(5 * time.Millisecond)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   body.Model,
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "pong"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

func newTestServer(t *testing.T, upstream string, apiKey string) (*httptest.Server, *metrics.Recorder, *ring.Buffer) {
	t.Helper()
	rec := metrics.NewRecorder()
	ring := ring.New(500)
	red := redactor.New()
	srv := New(Config{
		UpstreamBaseURL: upstream,
		UpstreamAPIKey:  apiKey,
		BufferSize:      500,
	}, rec, ring, red)
	ts := httptest.NewServer(srv.Handler())
	return ts, rec, ring
}

func TestNonStreamingProxy(t *testing.T) {
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	proxy, rec, ring := newTestServer(t, upstream.URL, "sk-test")
	defer proxy.Close()

	body := bytes.NewBufferString(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "pong") {
		t.Fatalf("missing upstream response: %s", respBody)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("metrics: want 1, got %d", len(snap))
	}
	if snap[0].OutputTok < 1 || snap[0].PromptTok < 1 {
		t.Fatalf("tokens not counted: %+v", snap[0])
	}
	if snap[0].Stream {
		t.Fatal("non-streaming should not be flagged stream")
	}
	bsnap := ring.Snapshot()
	if len(bsnap) < 2 {
		t.Fatalf("buffer should have >=2 entries, got %d", len(bsnap))
	}
}

func TestStreamingProxy(t *testing.T) {
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	proxy, rec, ring := newTestServer(t, upstream.URL, "sk-test")
	defer proxy.Close()

	body := bytes.NewBufferString(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type: want text/event-stream, got %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024), 1<<20)
	var collected []string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if d == "[DONE]" {
				break
			}
			collected = append(collected, d)
		}
	}
	if len(collected) < 2 {
		t.Fatalf("expected >=2 streamed chunks, got %d (%v)", len(collected), collected)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || !snap[0].Stream {
		t.Fatalf("stream metrics wrong: %+v", snap)
	}
	if snap[0].TTFT <= 0 {
		t.Fatalf("TTFT should be > 0, got %v", snap[0].TTFT)
	}
	if snap[0].OutputTok < 2 {
		t.Fatalf("output tokens should be >=2, got %d", snap[0].OutputTok)
	}
	bsnap := ring.Snapshot()
	if len(bsnap) < 2 {
		t.Fatalf("buffer entries: want >=2, got %d", len(bsnap))
	}
}

func TestConcurrentStreamRequests(t *testing.T) {
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	proxy, rec, _ := newTestServer(t, upstream.URL, "sk-test")
	defer proxy.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := bytes.NewBufferString(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
			if err != nil {
				t.Errorf("concurrent err: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if got := len(rec.Snapshot()); got != 8 {
		t.Fatalf("metrics count: want 8, got %d", got)
	}
}

func TestRedactsAPIKeyInBuffer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer upstream.Close()
	rec := metrics.NewRecorder()
	ring := ring.New(500)
	red := redactor.New()
	srv := New(Config{UpstreamBaseURL: upstream.URL, BufferSize: 500}, rec, ring, red)
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := bytes.NewBufferString(`{"model":"gpt-x","api_key":"sk-abcdefghijklmnopqrstuv","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, e := range ring.Snapshot() {
		if strings.Contains(e.Content, "sk-abcdefghijklmnopqrstuv") {
			t.Fatalf("unredacted secret in buffer: %q", e.Content)
		}
	}
}

func TestRootEndpoint(t *testing.T) {
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	proxy, _, _ := newTestServer(t, upstream.URL, "")
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var info map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&info)
	if info["service"] != "llm-top" {
		t.Fatalf("missing service name: %v", info)
	}
}

func TestBadUpstreamReturns502(t *testing.T) {
	rec := metrics.NewRecorder()
	ring := ring.New(500)
	red := redactor.New()
	srv := New(Config{UpstreamBaseURL: "http://127.0.0.1:1", BufferSize: 500}, rec, ring, red)
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := bytes.NewBufferString(`{"model":"x","messages":[]}`)
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: want 502, got %d", resp.StatusCode)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Err == "" {
		t.Fatalf("expected recorded error: %+v", snap)
	}
}

func TestCheckAvailable(t *testing.T) {
	if err := CheckAvailable("127.0.0.1:0"); err != nil {
		t.Fatalf("port 0 should be available: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	err = CheckAvailable(ln.Addr().String())
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("want ErrPortInUse, got %v", err)
	}
}

// stubStore is a RequestInserter that captures every Request sent to it.
type stubStore struct {
	mu      sync.Mutex
	records []Request
}

func (s *stubStore) Insert(r Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	return nil
}

func (s *stubStore) Snapshot() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.records))
	copy(out, s.records)
	return out
}

func TestEmitsXRequestIDHeader(t *testing.T) {
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	proxy, _, _ := newTestServer(t, upstream.URL, "sk-test")
	defer proxy.Close()

	body := bytes.NewBufferString(`{"model":"gpt-5.5-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	id := resp.Header.Get("X-Request-Id")
	if len(id) != 16 {
		t.Fatalf("X-Request-Id: want 16 hex chars, got %q (len %d)", id, len(id))
	}
}

func TestHonorsClientXRequestID(t *testing.T) {
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	proxy, _, _ := newTestServer(t, upstream.URL, "sk-test")
	defer proxy.Close()

	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"gpt-5.5-mini","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Request-Id", "client-supplied-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "client-supplied-id" {
		t.Fatalf("X-Request-Id: want client-supplied-id, got %q", got)
	}
}

func TestPersistsStructuredRequest(t *testing.T) {
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	rec := metrics.NewRecorder()
	rb := ring.New(500)
	red := redactor.New()
	srv := New(Config{UpstreamBaseURL: upstream.URL, UpstreamAPIKey: "sk-test", BufferSize: 500}, rec, rb, red)
	st := &stubStore{}
	srv.WithStore(st)
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Non-streaming.
	body := bytes.NewBufferString(`{"model":"gpt-5.5-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Streaming.
	body = bytes.NewBufferString(`{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err = http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Upstream error.
	body = bytes.NewBufferString(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	srv2 := New(Config{UpstreamBaseURL: "http://127.0.0.1:1", BufferSize: 500}, rec, rb, red)
	st2 := &stubStore{}
	srv2.WithStore(st2)
	proxy2 := httptest.NewServer(srv2.Handler())
	defer proxy2.Close()
	resp, err = http.Post(proxy2.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	records := st.Snapshot()
	if len(records) != 2 {
		t.Fatalf("stub-store: want 2 records, got %d", len(records))
	}
	for _, r := range records {
		if r.ID == "" || len(r.ID) != 16 {
			t.Errorf("bad id: %q", r.ID)
		}
		if r.StartedAt.IsZero() {
			t.Errorf("zero started_at: %+v", r)
		}
		if r.StatusCode != 200 {
			t.Errorf("bad status: %d", r.StatusCode)
		}
		if r.Model == "" {
			t.Errorf("missing model: %+v", r)
		}
		if r.Provider != "local" {
			t.Errorf("provider should be 'local' for 127.0.0.1 upstream, got %q", r.Provider)
		}
		if strings.Contains(r.RequestBody, "sk-test") {
			t.Errorf("api key leaked into RequestBody: %q", r.RequestBody)
		}
	}
	// Stream flag should be set on exactly one.
	gotStreams := 0
	for _, r := range records {
		if r.Stream {
			gotStreams++
		}
	}
	if gotStreams != 1 {
		t.Errorf("want 1 streaming record, got %d", gotStreams)
	}

	// Cost should be computed for both models.
	if records[0].CostUSD == 0 {
		t.Errorf("cost not computed for gpt-5.5-mini: %+v", records[0])
	}
	if records[1].CostUSD == 0 {
		t.Errorf("cost not computed for claude-sonnet-5: %+v", records[1])
	}

	// Error path also persists.
	errRecords := st2.Snapshot()
	if len(errRecords) != 1 {
		t.Fatalf("error stub-store: want 1, got %d", len(errRecords))
	}
	if errRecords[0].StatusCode != http.StatusBadGateway {
		t.Errorf("error status: want 502, got %d", errRecords[0].StatusCode)
	}
	if errRecords[0].Error == "" {
		t.Errorf("error message missing: %+v", errRecords[0])
	}
}

func TestSessionIDPropagatedToRecords(t *testing.T) {
	// Captures 3 requests through a proxy configured with SessionID
	// and asserts every persisted Request carries the same SessionID.
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	rec := metrics.NewRecorder()
	rb := ring.New(500)
	red := redactor.New()
	srv := New(Config{
		UpstreamBaseURL: upstream.URL,
		UpstreamAPIKey:  "sk-test",
		BufferSize:      500,
		SessionID:       "sess-test-001",
	}, rec, rb, red)
	st := &stubStore{}
	srv.WithStore(st)
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	for i := 0; i < 3; i++ {
		body := bytes.NewBufferString(`{"model":"gpt-5.5-mini","messages":[{"role":"user","content":"hi"}]}`)
		resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	records := st.Snapshot()
	if len(records) != 3 {
		t.Fatalf("want 3 records, got %d", len(records))
	}
	for _, r := range records {
		if r.SessionID != "sess-test-001" {
			t.Errorf("SessionID lost: %+v", r)
		}
	}
}

func TestEmptySessionIDDoesNotStamp(t *testing.T) {
	// When the proxy is started without --session, captured requests
	// should have empty SessionID (so session-scoped queries don't
	// accidentally match unrelated traffic).
	upstream := startFakeUpstream(t)
	defer upstream.Close()
	rec := metrics.NewRecorder()
	rb := ring.New(500)
	red := redactor.New()
	srv := New(Config{
		UpstreamBaseURL: upstream.URL,
		UpstreamAPIKey:  "sk-test",
		BufferSize:      500,
		// SessionID intentionally empty.
	}, rec, rb, red)
	st := &stubStore{}
	srv.WithStore(st)
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := bytes.NewBufferString(`{"model":"gpt-5.5-mini","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	records := st.Snapshot()
	if len(records) != 1 {
		t.Fatalf("want 1, got %d", len(records))
	}
	if records[0].SessionID != "" {
		t.Errorf("empty config SessionID leaked into record: %+v", records[0])
	}
}
