// Example: drive llm-top from your own code.
//
// This program starts a one-shot HTTP server on 127.0.0.1:8081 that
// proxies to whatever upstream you've configured, fires three
// chat-completion requests through it, and prints the resulting metrics
// snapshot. Useful as a starting point for embedding llm-top in tests
// or in CI.
//
// Usage:
//
//	go run ./examples/drive_embedded.go
//
// Environment variables (all optional):
//
//	LLMTOP_UPSTREAM   default https://api.openai.com
//	LLMTOP_API_KEY    default $OPENAI_API_KEY (only used if set)
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
	"github.com/aniketkarne-com/llm-top/internal/proxy"
	"github.com/aniketkarne-com/llm-top/internal/redactor"
)

func main() {
	upstream := os.Getenv("LLMTOP_UPSTREAM")
	if upstream == "" {
		upstream = "https://api.openai.com"
	}
	apiKey := os.Getenv("LLMTOP_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	rec := metrics.NewRecorder()
	buf := ring.New(100)
	red := redactor.New()

	// Bind to an OS-chosen port for this example.
	srv := proxy.New(proxy.Config{
		ListenAddr:      "127.0.0.1:0", // ignored — we use the listener below
		UpstreamBaseURL: upstream,
		UpstreamAPIKey:  apiKey,
		BufferSize:      100,
	}, rec, buf, red)

	// Set up an http.Server with a fixed port.
	httpSrv := &http.Server{
		Addr:    "127.0.0.1:8081",
		Handler: srv.Handler(),
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "server:", err)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()
	time.Sleep(100 * time.Millisecond) // let the server bind

	// Fire three requests.
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello #%d"}]}`, i+1)
		resp, err := http.Post("http://127.0.0.1:8081/v1/chat/completions",
			"application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "request %d: %v\n", i, err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// Print the metrics snapshot.
	summary := rec.Summarize()
	fmt.Printf("\nRequests: %d\n", summary.Count)
	fmt.Printf("Output tokens (est): %d\n", summary.TotalOutTok)
	fmt.Printf("Avg TTFT: %s\n", summary.AvgTTFT)
	fmt.Printf("P95 total: %s\n", summary.P95Total)
	fmt.Printf("Errors: %d\n", summary.Errors)
}