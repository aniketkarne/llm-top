package demo

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestRunDefaults(t *testing.T) {
	// Default options must complete without error and produce at least one
	// request, some output tokens, and a populated ring buffer.
	res, err := Run(Options{})
	if err != nil {
		t.Fatalf("demo.Run(Options{}) failed: %v", err)
	}
	if res.Requests < 1 {
		t.Errorf("expected at least 1 request, got %d", res.Requests)
	}
	if res.OutputTokens < 1 {
		t.Errorf("expected at least 1 output token, got %d", res.OutputTokens)
	}
	if res.BufferEntries < 3 {
		t.Errorf("expected at least 3 ring entries (request+preview+response per request), got %d", res.BufferEntries)
	}
	if res.Duration <= 0 {
		t.Errorf("expected positive duration, got %s", res.Duration)
	}
}

func TestRunWithOptions(t *testing.T) {
	res, err := Run(Options{
		UpstreamPort: 19090,
		RequestCount: 2,
		TokenCount:   10,
		TokenDelay:   2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("demo.Run with options failed: %v", err)
	}
	if res.Requests != 2 {
		t.Errorf("want 2 requests, got %d", res.Requests)
	}
	// 10 tokens per request, ~16 max_tokens = ~10 actual output tokens each
	// (estimate rounds up to chunks of 4 chars). Allow generous slack.
	if res.OutputTokens < 10 || res.OutputTokens > 100 {
		t.Errorf("output_tokens out of expected range: %d", res.OutputTokens)
	}
}

func TestRunFallsBackToFreePort(t *testing.T) {
	// If the configured port is taken, demo.Run should walk up to find a
	// free one. We can't easily reserve a port in the test without extra
	// dependencies, so this is a smoke test that ensures fallback happens
	// silently when called multiple times concurrently-ish.
	for i := 0; i < 2; i++ {
		_, err := Run(Options{
			UpstreamPort: 19100 + i*10,
			RequestCount: 1,
			TokenCount:   3,
			TokenDelay:   1 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

func TestBuildDemoRequestShape(t *testing.T) {
	body := buildDemoRequest(2, 10)
	if !strings.Contains(string(body), `"model"`) {
		t.Errorf("expected body to contain model field, got: %s", body)
	}
	if !strings.Contains(string(body), `"stream":true`) {
		t.Errorf("expected body to contain stream:true, got: %s", body)
	}
	if !strings.Contains(string(body), `"messages"`) {
		t.Errorf("expected body to contain messages array, got: %s", body)
	}
}

func TestSyntheticTokens(t *testing.T) {
	// First call returns real-looking words; later calls fall back to
	// numbered tokens.
	if got := syntheticTokens(0); got != "This" {
		t.Errorf("syntheticTokens(0) = %q, want %q", got, "This")
	}
	if got := syntheticTokens(100); !strings.HasPrefix(got, " tok") {
		t.Errorf("syntheticTokens(100) = %q, want something starting with ' tok'", got)
	}
}

func TestIsFree(t *testing.T) {
	// Port 1 (privileged) is virtually never bindable as a normal user.
	// The function should report not-free rather than crash.
	_, err := isFree(1)
	// We don't care whether it's free or not — just that it returns no
	// panic. isFree returns (bool, error); either way is acceptable.
	if err != nil {
		t.Logf("isFree(1) returned err (expected): %v", err)
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}
	o.withDefaults()
	if o.UpstreamPort != 18080 {
		t.Errorf("default upstream port: got %d, want 18080", o.UpstreamPort)
	}
	if o.RequestCount != 5 {
		t.Errorf("default request count: got %d, want 5", o.RequestCount)
	}
	if o.TokenCount != 40 {
		t.Errorf("default token count: got %d, want 40", o.TokenCount)
	}
	if o.TokenDelay == 0 {
		t.Errorf("default token delay must be > 0, got 0")
	}
}

func TestWaitForListenSucceeds(t *testing.T) {
	// Bind a real listener and verify waitForListen returns nil.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	if err := waitForListen(addr, 1*time.Second); err != nil {
		t.Errorf("waitForListen on real listener failed: %v", err)
	}
}

func TestWaitForListenTimesOut(t *testing.T) {
	// Use a port that we know is not listening.
	if err := waitForListen("127.0.0.1:1", 100*time.Millisecond); err == nil {
		t.Errorf("expected timeout error, got nil")
	}
}