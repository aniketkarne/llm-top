// Tests for the rolling-baseline anomaly Detector. Each detector is
// exercised with synthetic time series, edge cases, and the
// concurrency safety property (`go test -race`).
package anomaly

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// fakeClock returns a deterministic clock that advances by 1ms on
// every call. The increment is large enough to push successive
// requests past any time-window eviction without requiring the test
// to actually sleep.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func (f *fakeClock) asFunc() func() time.Time {
	return f.now
}

// req is a tiny builder so tests stay readable. Zeros are valid; we
// only set the fields that matter for each test.
func req(id string, model string) proxy.Request {
	return proxy.Request{
		ID:        id,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC(),
		Model:     model,
		Method:    "POST",
		Path:      "/v1/chat/completions",
		StatusCode: 200,
	}
}

// okReq is a successful request with an explicit provider. Used by
// tests that mix OK and error traffic to the same provider key.
func okReq(id, model, provider string) proxy.Request {
	r := req(id, model)
	r.Provider = provider
	return r
}

// streamReq sets Stream=true and TTFTMillis.
func streamReq(id, model string, ttft, total int64) proxy.Request {
	r := req(id, model)
	r.Stream = true
	r.TTFTMillis = ttft
	r.TotalMillis = total
	return r
}

// jsonReq sets TotalMillis only (no TTFT).
func jsonReq(id, model string, total int64) proxy.Request {
	r := req(id, model)
	r.Stream = false
	r.TotalMillis = total
	return r
}

// errReq sets Error/StatusCode. The threshold detectors (ErrorBurst,
// ProviderDegrade) treat either as an error.
func errReq(id, model, provider string, status int, msg string) proxy.Request {
	r := req(id, model)
	r.Provider = provider
	r.StatusCode = status
	if msg != "" {
		r.Error = msg
	}
	return r
}

// -- Detector: defaults -------------------------------------------------------

func TestNewDefaults(t *testing.T) {
	d := New()
	if d.TTFTMultiplier != 3.0 {
		t.Errorf("TTFTMultiplier default: want 3.0, got %v", d.TTFTMultiplier)
	}
	if d.RepeatedPromptCount != 3 {
		t.Errorf("RepeatedPromptCount default: want 3, got %d", d.RepeatedPromptCount)
	}
	if d.ErrorBurstThreshold != 0.25 {
		t.Errorf("ErrorBurstThreshold default: want 0.25, got %v", d.ErrorBurstThreshold)
	}
	if d.LargeContextTokens != 32000 {
		t.Errorf("LargeContextTokens default: want 32000, got %d", d.LargeContextTokens)
	}
	if d.WindowSize != 100 {
		t.Errorf("WindowSize default: want 100, got %d", d.WindowSize)
	}
	if d.Clock == nil {
		t.Errorf("Clock default: want non-nil")
	}
}

// -- LargeContext ------------------------------------------------------------

func TestLargeContextFiresAboveThreshold(t *testing.T) {
	d := New()
	r := req("r1", "gpt-5.5")
	r.PromptTokens = 50000 // default threshold is 32000
	got := d.Evaluate(r)
	if !hasKind(got, KindLargeContext) {
		t.Fatalf("expected LargeContext anomaly, got %v", got)
	}
}

func TestLargeContextSilentBelowThreshold(t *testing.T) {
	d := New()
	r := req("r1", "gpt-5.5")
	r.PromptTokens = 1000
	got := d.Evaluate(r)
	if hasKind(got, KindLargeContext) {
		t.Fatalf("unexpected LargeContext anomaly: %v", got)
	}
}

func TestLargeContextExactThresholdSilent(t *testing.T) {
	// We fire strictly > threshold. Boundary check.
	d := New()
	r := req("r1", "gpt-5.5")
	r.PromptTokens = d.LargeContextTokens
	got := d.Evaluate(r)
	if hasKind(got, KindLargeContext) {
		t.Fatalf("threshold must be strict: %v", got)
	}
}

// -- TTFT spike --------------------------------------------------------------

func TestTTFTSpikeFiresAboveRollingP95(t *testing.T) {
	d := New()
	// Feed 30 baseline streaming requests at 200ms TTFT, then a 2000ms spike.
	for i := 0; i < 30; i++ {
		d.Evaluate(streamReq(fmt.Sprintf("base-%d", i), "gpt-5.5", 200, 1000))
	}
	got := d.Evaluate(streamReq("spike-1", "gpt-5.5", 2000, 5000))
	if !hasKind(got, KindTTFTSpike) {
		t.Fatalf("expected TTFT spike, got %v", got)
	}
	a := findKind(got, KindTTFTSpike)
	if a.RequestID != "spike-1" {
		t.Errorf("RequestID: want spike-1, got %s", a.RequestID)
	}
}

func TestTTFTSpikeSilentForFirstRequests(t *testing.T) {
	// Until the baseline buffer reaches its minimum (10), no spike fires.
	d := New()
	for i := 0; i < 5; i++ {
		got := d.Evaluate(streamReq(fmt.Sprintf("r-%d", i), "gpt-5.5", 5000, 5000))
		if hasKind(got, KindTTFTSpike) {
			t.Fatalf("spike fired on warm-up req %d: %v", i, got)
		}
	}
}

func TestTTFTSpikeOnlyStreaming(t *testing.T) {
	// A non-streaming request never feeds the TTFT ring buffer, even if
	// the caller set TTFTMillis > 0. This prevents a degenerate case
	// where every JSON response poisons the baseline.
	d := New()
	for i := 0; i < 30; i++ {
		// Non-streaming: TTFT ignored.
		d.Evaluate(streamReq(fmt.Sprintf("base-%d", i), "gpt-5.5", 0, 1000))
	}
	r := jsonReq("n1", "gpt-5.5", 5000)
	r.TTFTMillis = 9999 // would be a spike if it counted
	got := d.Evaluate(r)
	if hasKind(got, KindTTFTSpike) {
		t.Fatalf("non-streaming request should never trigger TTFT spike: %v", got)
	}
}

func TestTTFTSpikeSilentWhenWithinMultiplier(t *testing.T) {
	d := New()
	for i := 0; i < 30; i++ {
		d.Evaluate(streamReq(fmt.Sprintf("base-%d", i), "gpt-5.5", 200, 1000))
	}
	// 600ms is 3x baseline but we compare with strict ">", and 600 == 3*200.
	// Use 599 to be safely below.
	got := d.Evaluate(streamReq("r1", "gpt-5.5", 599, 1000))
	if hasKind(got, KindTTFTSpike) {
		t.Fatalf("within-multiplier value should not spike: %v", got)
	}
}

// -- Latency spike -----------------------------------------------------------

func TestLatencySpikeFiresAboveRollingP95(t *testing.T) {
	d := New()
	for i := 0; i < 30; i++ {
		d.Evaluate(streamReq(fmt.Sprintf("base-%d", i), "gpt-5.5", 200, 1000))
	}
	got := d.Evaluate(streamReq("slow", "gpt-5.5", 200, 10000))
	if !hasKind(got, KindLatencySpike) {
		t.Fatalf("expected latency spike, got %v", got)
	}
}

func TestLatencySpikeBothStreamAndJSON(t *testing.T) {
	d := New()
	// Mix streaming + JSON baselines (they share the same per-model buffer).
	for i := 0; i < 15; i++ {
		d.Evaluate(streamReq(fmt.Sprintf("s-%d", i), "gpt-5.5", 200, 1000))
	}
	for i := 0; i < 15; i++ {
		d.Evaluate(jsonReq(fmt.Sprintf("j-%d", i), "gpt-5.5", 1000))
	}
	got := d.Evaluate(jsonReq("j-spike", "gpt-5.5", 8000))
	if !hasKind(got, KindLatencySpike) {
		t.Fatalf("JSON latency spike expected: %v", got)
	}
}

// -- Token explosion ---------------------------------------------------------

func TestTokenExplosionFiresAboveRollingMean(t *testing.T) {
	d := New()
	// 5 baseline requests with mean 100 tokens.
	for i := 0; i < 5; i++ {
		r := streamReq(fmt.Sprintf("base-%d", i), "gpt-5.5", 200, 1000)
		r.OutputTokens = 100
		d.Evaluate(r)
	}
	// 1000 tokens is 10x the mean; threshold is 3x.
	r := streamReq("big", "gpt-5.5", 200, 5000)
	r.OutputTokens = 1000
	got := d.Evaluate(r)
	if !hasKind(got, KindTokenExplosion) {
		t.Fatalf("expected token explosion, got %v", got)
	}
}

func TestTokenExplosionNeedsMinimumSamples(t *testing.T) {
	d := New()
	// 4 baseline samples (below the 5-sample threshold); spike should NOT fire.
	for i := 0; i < 4; i++ {
		r := streamReq(fmt.Sprintf("base-%d", i), "gpt-5.5", 200, 1000)
		r.OutputTokens = 100
		d.Evaluate(r)
	}
	r := streamReq("big", "gpt-5.5", 200, 5000)
	r.OutputTokens = 99999
	got := d.Evaluate(r)
	if hasKind(got, KindTokenExplosion) {
		t.Fatalf("token explosion should need >= 5 samples: %v", got)
	}
}

// -- Repeated prompt ---------------------------------------------------------

func TestRepeatedPromptFiresOnThreshold(t *testing.T) {
	clk := newFakeClock()
	d := New()
	d.Clock = clk.asFunc()
	const hash = "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"
	for i := 0; i < 3; i++ {
		r := req(fmt.Sprintf("r-%d", i), "gpt-5.5")
		r.PromptHash = hash
		got := d.Evaluate(r)
		// Only the 3rd request should fire.
		shouldFire := i == 2
		if hasKind(got, KindRepeatedPrompt) != shouldFire {
			t.Errorf("req %d: expected fire=%v, got=%v", i, shouldFire, got)
		}
	}
}

func TestRepeatedPromptFiresOnceNotEveryTime(t *testing.T) {
	clk := newFakeClock()
	d := New()
	d.Clock = clk.asFunc()
	const hash = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	for i := 0; i < 6; i++ {
		r := req(fmt.Sprintf("r-%d", i), "gpt-5.5")
		r.PromptHash = hash
		got := d.Evaluate(r)
		// Only the 3rd hits the threshold (count == RepeatedPromptCount).
		if i == 2 && !hasKind(got, KindRepeatedPrompt) {
			t.Errorf("req %d: expected fire, got %v", i, got)
		}
		if i != 2 && hasKind(got, KindRepeatedPrompt) {
			t.Errorf("req %d: unexpected fire (already triggered): %v", i, got)
		}
	}
}

func TestRepeatedPromptWindowExpiry(t *testing.T) {
	clk := newFakeClock()
	d := New()
	d.Clock = clk.asFunc()
	const hash = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	// First two requests close in time.
	r1 := req("r1", "gpt-5.5")
	r1.PromptHash = hash
	d.Evaluate(r1)
	clk.advance(10 * time.Second)
	r2 := req("r2", "gpt-5.5")
	r2.PromptHash = hash
	d.Evaluate(r2)
	// Advance past the window (60s default).
	clk.advance(120 * time.Second)
	r3 := req("r3", "gpt-5.5")
	r3.PromptHash = hash
	got := d.Evaluate(r3)
	// r1 should have aged out; count is 2.
	if hasKind(got, KindRepeatedPrompt) {
		t.Fatalf("expected no anomaly after window expiry, got %v", got)
	}
}

// -- Error burst -------------------------------------------------------------

func TestErrorBurstFiresAtThreshold(t *testing.T) {
	d := New()
	// ErrorBurstWindow default is 20, threshold 25% (strict >).
	// With 14 OK + 5 errors = 19 reqs in the window, the 6th error
	// (the 20th request overall) pushes us from 25% to 30%.
	for i := 0; i < 14; i++ {
		d.Evaluate(okReq(fmt.Sprintf("ok-%d", i), "gpt-5.5", "openai"))
	}
	var got []Anomaly
	for i := 0; i < 5; i++ {
		got = append(got, d.Evaluate(errReq(fmt.Sprintf("err-pre-%d", i), "gpt-5.5", "openai", 500, "boom"))...)
	}
	got = append(got, d.Evaluate(errReq("err-final", "gpt-5.5", "openai", 500, "boom"))...)
	if !hasKind(got, KindErrorBurst) {
		t.Fatalf("expected error burst somewhere in %d anomalies, got %v", len(got), got)
	}
	a := findKind(got, KindErrorBurst)
	if a.Severity != SeverityAlert {
		t.Errorf("severity: want alert, got %s", a.Severity)
	}
}

func TestErrorBurstOnlyOnRisingEdge(t *testing.T) {
	// Once the burst is in flight, additional errors should NOT re-fire
	// every single time. Otherwise the alert spam kills the log.
	d := New()
	var allAnomalies []Anomaly
	for i := 0; i < 25; i++ {
		allAnomalies = append(allAnomalies, d.Evaluate(errReq(fmt.Sprintf("e-%d", i), "gpt-5.5", "openai", 500, "boom"))...)
	}
	count := 0
	for _, a := range allAnomalies {
		if a.Kind == KindErrorBurst {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 ErrorBurst across 25 errors, got %d", count)
	}
}

func TestErrorBurstSilentWhenBelowThreshold(t *testing.T) {
	d := New()
	for i := 0; i < 20; i++ {
		d.Evaluate(okReq(fmt.Sprintf("ok-%d", i), "gpt-5.5", "openai"))
	}
	// Even with one error at the end, 1/20 = 5% < 25%.
	got := d.Evaluate(errReq("e1", "gpt-5.5", "openai", 500, "boom"))
	if hasKind(got, KindErrorBurst) {
		t.Fatalf("low error rate should not fire: %v", got)
	}
}

// -- Provider degrade --------------------------------------------------------

func TestProviderDegradeFiresWhenOneProviderErrorsMore(t *testing.T) {
	d := New()
	// Provider A: 5/5 errors. Provider B: 0/5 errors. Same model.
	for i := 0; i < 5; i++ {
		d.Evaluate(errReq(fmt.Sprintf("a-%d", i), "gpt-5.5", "openai", 500, "boom"))
	}
	for i := 0; i < 5; i++ {
		d.Evaluate(req(fmt.Sprintf("b-%d", i), "gpt-5.5")) // openai provider default
	}
	// Now switch one request to a different provider — wait, we need to
	// override Provider on the success-path requests.
	// Re-run with explicit provider on success:
	d2 := New()
	for i := 0; i < 5; i++ {
		d2.Evaluate(errReq(fmt.Sprintf("a-%d", i), "gpt-5.5", "openai", 500, "boom"))
	}
	for i := 0; i < 5; i++ {
		r := req(fmt.Sprintf("b-%d", i), "gpt-5.5")
		r.Provider = "anthropic"
		d2.Evaluate(r)
	}
	// One more openai error pushes the rate and lets ProviderDegrade
	// compare this 100% provider against the 0% provider.
	got := d2.Evaluate(errReq("a-final", "gpt-5.5", "openai", 500, "boom"))
	if !hasKind(got, KindProviderDegrade) {
		t.Fatalf("expected provider degrade, got %v", got)
	}
}

func TestProviderDegradeSilentWhenBothProvidersHealthy(t *testing.T) {
	d := New()
	for i := 0; i < 10; i++ {
		r := req(fmt.Sprintf("ok-%d", i), "gpt-5.5")
		r.Provider = "openai"
		d.Evaluate(r)
	}
	for i := 0; i < 10; i++ {
		r := req(fmt.Sprintf("ok-%d", i), "gpt-5.5")
		r.Provider = "anthropic"
		d.Evaluate(r)
	}
	// No anomalies expected for healthy traffic.
	got := d.Recent(20)
	if len(got) != 0 {
		t.Fatalf("healthy traffic should produce no anomalies: %v", got)
	}
}

// -- Rolling window correctness ----------------------------------------------

func TestRollingWindowEviction(t *testing.T) {
	d := New()
	d.WindowSize = 5
	// Push 10 samples; only the last 5 should remain in the buffer.
	for i := 0; i < 10; i++ {
		d.Evaluate(streamReq(fmt.Sprintf("r-%d", i), "gpt-5.5", int64(100+i), 1000))
	}
	d.mu.Lock()
	buf := d.ttftByModel["gpt-5.5"]
	vals := buf.values()
	d.mu.Unlock()
	if len(vals) != 5 {
		t.Fatalf("ring size: want 5, got %d", len(vals))
	}
	if vals[0] != 105 || vals[4] != 109 {
		t.Errorf("expected last-5 samples [105..109], got %v", vals)
	}
}

func TestBaselineAccumulationPreventsFalsePositive(t *testing.T) {
	// 100 requests with constant 100ms TTFT. The 101st request at
	// 200ms is double but still inside the multiplier. Should NOT fire.
	d := New()
	for i := 0; i < 100; i++ {
		d.Evaluate(streamReq(fmt.Sprintf("r-%d", i), "gpt-5.5", 100, 500))
	}
	got := d.Evaluate(streamReq("r-100", "gpt-5.5", 200, 500))
	if hasKind(got, KindTTFTSpike) {
		t.Fatalf("2x baseline should not fire with 3x threshold: %v", got)
	}
}

// -- Concurrency safety ------------------------------------------------------

func TestEvaluateConcurrencySafety(t *testing.T) {
	// 100 goroutines, 100 requests each. Run with -race to catch any
	// unprotected state mutation. The Detector must never deadlock,
	// panic, or trigger a race-detector report.
	d := New()
	const goroutines = 100
	const perGoroutine = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				r := streamReq(fmt.Sprintf("g%d-r%d", g, i), "gpt-5.5", int64(100+i%50), 1000)
				r.PromptHash = fmt.Sprintf("hash-%d", i%5)
				d.Evaluate(r)
			}
		}(g)
	}
	wg.Wait()
	// Recent should return a bounded slice without panicking.
	got := d.Recent(10)
	if len(got) > 1000 {
		t.Errorf("anomaly ring exceeded cap: %d", len(got))
	}
}

// -- Recent accessor ---------------------------------------------------------

func TestRecentNewestFirst(t *testing.T) {
	d := New()
	for i := 0; i < 5; i++ {
		r := req(fmt.Sprintf("r%d", i), "gpt-5.5")
		r.PromptTokens = 100000 // always fires LargeContext
		d.Evaluate(r)
	}
	got := d.Recent(3)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	// The most-recent Evaluate produced the last anomaly; it should be first.
	if !strings.HasPrefix(got[0].RequestID, "r4") {
		t.Errorf("expected r4 first, got %s", got[0].RequestID)
	}
}

func TestRecentEmpty(t *testing.T) {
	d := New()
	got := d.Recent(10)
	if len(got) != 0 {
		t.Errorf("expected empty, got %d anomalies", len(got))
	}
}

func TestRecentNZeroOrNegative(t *testing.T) {
	d := New()
	d.Evaluate(req("r1", "gpt-5.5"))
	if got := d.Recent(0); got != nil {
		t.Errorf("Recent(0): want nil, got %v", got)
	}
	if got := d.Recent(-1); got != nil {
		t.Errorf("Recent(-1): want nil, got %v", got)
	}
}

// -- EncodeExtra -------------------------------------------------------------

func TestEncodeExtra(t *testing.T) {
	tests := []struct {
		name string
		a    Anomaly
		want string
	}{
		{
			name: "nil map",
			a:    Anomaly{},
			want: "{}",
		},
		{
			name: "empty map",
			a:    Anomaly{Extra: map[string]string{}},
			want: "{}",
		},
		{
			name: "populated",
			a:    Anomaly{Extra: map[string]string{"k": "v"}},
			want: `{"k":"v"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.EncodeExtra()
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// -- Helpers -----------------------------------------------------------------

func hasKind(anomalies []Anomaly, k Kind) bool {
	for _, a := range anomalies {
		if a.Kind == k {
			return true
		}
	}
	return false
}

func findKind(anomalies []Anomaly, k Kind) Anomaly {
	for _, a := range anomalies {
		if a.Kind == k {
			return a
		}
	}
	return Anomaly{}
}