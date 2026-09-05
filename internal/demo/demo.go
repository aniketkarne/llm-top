// Package demo runs an end-to-end showcase of llm-top with zero external
// dependencies, no API key, and no network calls. It spins up:
//
//   1. A built-in HTTP server on 127.0.0.1:<port> that emits a synthetic
//      OpenAI-compatible SSE stream (chunks of plausible assistant text,
//      timed to mimic real TTFT and tokens-per-second).
//   2. A built-in HTTP client that fires a few POST /v1/chat/completions
//      requests at a real llm-top proxy Server bound to 127.0.0.1:<port+1>.
//   3. The proxy records metrics + ring-buffer entries as the requests flow.
//   4. A one-shot snapshot of the UI is rendered to stdout, demonstrating the
//      dashboard populated with the demo's traffic.
//
// Total runtime: typically <300ms including 5 synthetic requests. Exits
// non-zero if anything in the demo loop fails.
//
// The package is intentionally self-contained: it does not touch disk, does
// not require an OPENAI_API_KEY, and does not make any outbound HTTP calls.
package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
	"github.com/aniketkarne-com/llm-top/internal/proxy"
	"github.com/aniketkarne-com/llm-top/internal/redactor"
	"github.com/aniketkarne-com/llm-top/internal/ui"
)

// Options configures the demo. Zero values get sensible defaults.
type Options struct {
	// UpstreamPort is the port the synthetic OpenAI server binds to.
	// The proxy binds to UpstreamPort+1. Defaults to 18080.
	UpstreamPort int
	// RequestCount is how many synthetic chat-completion requests to fire.
	// Defaults to 5.
	RequestCount int
	// TokenDelay is the delay between emitted SSE chunks. Defaults to
	// 8ms which gives roughly the realistic ~125 tokens/sec on screen.
	TokenDelay time.Duration
	// TokenCount is how many synthetic tokens each response emits.
	// Defaults to 40.
	TokenCount int
}

func (o *Options) withDefaults() {
	if o.UpstreamPort == 0 {
		o.UpstreamPort = 18080
	}
	if o.RequestCount == 0 {
		o.RequestCount = 5
	}
	if o.TokenDelay == 0 {
		o.TokenDelay = 8 * time.Millisecond
	}
	if o.TokenCount == 0 {
		o.TokenCount = 40
	}
}

// Result summarizes what the demo did.
type Result struct {
	Requests      int
	TTFTTotalMs   int64
	OutputTokens  int
	PromptTokens  int
	Duration      time.Duration
	BufferEntries int
}

// Run executes the demo end-to-end and returns the result. The synthetic
// upstream, the proxy, and the client all live for the duration of the call;
// nothing persists.
func Run(o Options) (Result, error) {
	o.withDefaults()
	res := Result{}
	start := time.Now()

	// Free ports: walk up if the requested port is taken.
	upstreamPort := o.UpstreamPort
	for ; upstreamPort < o.UpstreamPort+20; upstreamPort++ {
		if free, err := isFree(upstreamPort); err == nil && free {
			break
		}
	}

	upstreamAddr := fmt.Sprintf("127.0.0.1:%d", upstreamPort)
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", upstreamPort+1)

	// Shared state wired into both proxy and UI.
	rec := metrics.NewRecorder()
	buf := ring.New(64)
	red := redactor.New()

	// Synthetic upstream.
	upCtx, upCancel := context.WithCancel(context.Background())
	defer upCancel()
	upstream := newSyntheticUpstream(upCtx, o.TokenCount, o.TokenDelay)
	upSrv := &http.Server{
		Addr:    upstreamAddr,
		Handler: upstream,
	}
	upErrCh := make(chan error, 1)
	go func() { upErrCh <- upSrv.ListenAndServe() }()
	defer func() { _ = upSrv.Shutdown(context.Background()) }()

	// Proxy pointing at the synthetic upstream.
	prx := proxy.New(proxy.Config{
		ListenAddr:      proxyAddr,
		UpstreamBaseURL: "http://" + upstreamAddr,
		UpstreamAPIKey:  "sk-demo-key", // never sent over the network (loopback)
		BufferSize:      64,
	}, rec, buf, red)
	prxCtx, prxCancel := context.WithCancel(context.Background())
	defer prxCancel()
	prxErrCh := make(chan error, 1)
	go func() { prxErrCh <- prx.ListenAndServe(prxCtx) }()

	// Wait for both servers to actually be listening.
	if err := waitForListen(upstreamAddr, 2*time.Second); err != nil {
		return res, fmt.Errorf("upstream did not start: %w", err)
	}
	if err := waitForListen(proxyAddr, 2*time.Second); err != nil {
		return res, fmt.Errorf("proxy did not start: %w", err)
	}

	// Fire synthetic requests.
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < o.RequestCount; i++ {
		body := buildDemoRequest(i, o.TokenCount)
		req, _ := http.NewRequest("POST", "http://"+proxyAddr+"/v1/chat/completions",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			return res, fmt.Errorf("request %d: %w", i, err)
		}
		// Drain the body so the connection is reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		res.Requests++
	}
	// Give the proxy recorder a tick to settle.
	time.Sleep(50 * time.Millisecond)

	// Snapshot metrics.
	summary := rec.Summarize()
	res.TTFTTotalMs = summary.AvgTTFT.Milliseconds() * int64(summary.Count)
	res.OutputTokens = summary.TotalOutTok
	res.PromptTokens = summary.TotalInTok
	res.BufferEntries = buf.Len()
	res.Duration = time.Since(start)

	// Render a UI snapshot so the user sees what the dashboard looks like.
	model := ui.New(rec, buf, prx, proxyAddr, "http://"+upstreamAddr)
	model.NoColor = false // demo runs against a TTY-style stdout
	fmt.Println(model.RenderSnapshot())

	// Tear down.
	prxCancel()
	upCancel()
	return res, nil
}

// buildDemoRequest constructs a minimal but plausible OpenAI chat-completion
// payload with stream=true.
func buildDemoRequest(idx, tokenCount int) []byte {
	payload := map[string]any{
		"model": "demo-model-1",
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant in a demo."},
			{"role": "user", "content": fmt.Sprintf("Demo request #%d (target=%d tokens)", idx+1, tokenCount)},
		},
		"stream":  true,
		"max_tokens": tokenCount + 16,
	}
	b, _ := json.Marshal(payload)
	return b
}

// syntheticUpstream is an http.Handler that returns OpenAI-compatible SSE
// streaming responses.
type syntheticUpstream struct {
	tokens     int
	chunkDelay time.Duration
}

func newSyntheticUpstream(ctx context.Context, tokens int, delay time.Duration) *syntheticUpstream {
	_ = ctx
	return &syntheticUpstream{tokens: tokens, chunkDelay: delay}
}

func (s *syntheticUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	// Read and discard request body — we don't actually inspect it.
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}

	for i := 0; i < s.tokens; i++ {
		tok := syntheticTokens(i)
		chunk := map[string]any{
			"id":      fmt.Sprintf("chatcmpl-demo-%d", i),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "demo-model-1",
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"content": tok}, "finish_reason": nil},
			},
		}
		b, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(s.chunkDelay)
	}
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// syntheticTokens emits plausible-looking tokens. It cycles through a small
// vocabulary so the response is recognisable but each chunk is distinct.
func syntheticTokens(i int) string {
	vocab := []string{
		"This", " is", " a", " synthetic", " llm-top", " demo", " response", ".",
		" It", " simulates", " streaming", " tokens", " so", " you", " can", " see",
		" TTFT", " and", " tokens/sec", " in", " the", " dashboard", " without",
		" any", " real", " upstream", " calls", " or", " API", " keys", ".",
	}
	if i < len(vocab) {
		return vocab[i]
	}
	return fmt.Sprintf(" tok%d", i)
}

func isFree(port int) (bool, error) {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false, err
	}
	_ = l.Close()
	return true, nil
}

func waitForListen(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("timeout waiting for " + addr)
}