package prom

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/anomaly"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
)

func TestHandlerEmitsAllExpectedMetricFamilies(t *testing.T) {
	rec := metrics.NewRecorder()
	now := time.Now()
	// One streaming success, one JSON success, one error.
	rec.Add(metrics.Record{
		Start: now, End: now.Add(800 * time.Millisecond),
		TTFT: 300 * time.Millisecond, Total: 800 * time.Millisecond,
		PromptTok: 100, OutputTok: 50,
		Model: "gpt-5.5", Provider: "openai", Status: 200, Stream: true,
	})
	rec.Add(metrics.Record{
		Start: now, End: now.Add(400 * time.Millisecond),
		Total:     400 * time.Millisecond,
		PromptTok: 80, OutputTok: 20,
		Model: "claude-sonnet-5", Provider: "anthropic", Status: 200, Stream: false,
	})
	rec.Add(metrics.Record{
		Start: now, End: now.Add(100 * time.Millisecond),
		Total:     100 * time.Millisecond,
		PromptTok: 10, OutputTok: 0,
		Model: "gpt-5.5", Provider: "openai", Status: 500, Err: "boom",
	})

	src := StaticSource(map[AnomalyKey]uint64{
		{Kind: anomaly.KindTTFTSpike, Severity: anomaly.SeverityWarn}: 1,
	})

	h := Handler(rec, src)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content-type: want text/plain*, got %q", got)
	}
	body := readAll(t, resp.Body)

	wantFamilies := []string{
		"# TYPE llm_requests_total counter",
		"# TYPE llm_errors_total counter",
		"# TYPE llm_input_tokens_total counter",
		"# TYPE llm_output_tokens_total counter",
		"# TYPE llm_cost_usd_total counter",
		"# TYPE llm_ttft_seconds histogram",
		"# TYPE llm_latency_seconds histogram",
		"# TYPE llm_anomalies_total counter",
		"# TYPE go_goroutines gauge",
	}
	for _, want := range wantFamilies {
		if !strings.Contains(body, want) {
			t.Errorf("missing metric family header: %q\nbody:\n%s", want, body)
		}
	}

	// Spot-check concrete values.
	wantLines := []string{
		`llm_requests_total{model="gpt-5.5",provider="openai",status="all"} 2`,
		`llm_requests_total{model="claude-sonnet-5",provider="anthropic",status="all"} 1`,
		`llm_input_tokens_total{model="gpt-5.5",provider="openai"} 110`,
		`llm_output_tokens_total{model="claude-sonnet-5",provider="anthropic"} 20`,
		`llm_errors_total{model="gpt-5.5",provider="openai",kind="5xx"} 1`,
		`llm_anomalies_total{kind="ttft_spike",severity="warn"} 1`,
	}
	for _, want := range wantLines {
		if !strings.Contains(body, want) {
			t.Errorf("missing expected line: %q", want)
		}
	}
}

func TestHistogramBucketBoundaries(t *testing.T) {
	rec := metrics.NewRecorder()
	now := time.Now()
	// A value of 0.3s should fall in 0.5s bucket but NOT in 0.25s.
	// A value of 5s should fall in the 5s bucket and everything below.
	// A value of 30s should fall only in +Inf.
	cases := []struct {
		total   time.Duration
		wantLE  string // exact bucket line expected to contain this observation
		notInLE string // exact bucket line that must NOT contain it
	}{
		{300 * time.Millisecond, `le="0.5"`, `le="0.25"`},
		{5 * time.Second, `le="5"`, `le="2.5"`},
		{30 * time.Second, `le="+Inf"`, `le="10"`},
		{6 * time.Millisecond, `le="0.01"`, `le="0.005"`}, // 6ms > 5ms, <= 10ms
	}

	for i, c := range cases {
		rec = metrics.NewRecorder()
		rec.Add(metrics.Record{
			Start: now, End: now.Add(c.total),
			Total: c.total,
			Model: "gpt-5.5", Provider: "openai", Status: 200,
		})
		body := scrapeBody(t, Handler(rec, StaticSource(nil)))
		// Look for the line with both the bucket and model/provider labels.
		wantLine := "llm_latency_seconds_bucket{model=\"gpt-5.5\",provider=\"openai\"," + c.wantLE + "} 1"
		notLine := "llm_latency_seconds_bucket{model=\"gpt-5.5\",provider=\"openai\"," + c.notInLE + "} 1"
		if !strings.Contains(body, wantLine) {
			t.Errorf("case %d (%v): expected line missing: %q\nbody:\n%s", i, c.total, wantLine, body)
		}
		if strings.Contains(body, notLine) {
			t.Errorf("case %d (%v): unexpected presence: %q\nbody:\n%s", i, c.total, notLine, body)
		}
	}
}

func TestHistogramCountAndSum(t *testing.T) {
	rec := metrics.NewRecorder()
	now := time.Now()
	// Two requests at 1s each -> count=2, sum=2.
	rec.Add(metrics.Record{Start: now, End: now.Add(time.Second), Total: time.Second, Model: "m", Provider: "openai", Status: 200})
	rec.Add(metrics.Record{Start: now, End: now.Add(time.Second), Total: time.Second, Model: "m", Provider: "openai", Status: 200})
	body := scrapeBody(t, Handler(rec, StaticSource(nil)))
	if !strings.Contains(body, `llm_latency_seconds_count{model="m",provider="openai"} 2`) {
		t.Errorf("count mismatch:\n%s", body)
	}
	if !strings.Contains(body, `llm_latency_seconds_sum{model="m",provider="openai"} 2`) {
		t.Errorf("sum mismatch:\n%s", body)
	}
	// Every bucket up to and including 1 should be 2; 2.5 should be 2 (because 1 <= 2.5); 0.5 should be 0.
	if !strings.Contains(body, `llm_latency_seconds_bucket{model="m",provider="openai",le="1"} 2`) {
		t.Errorf("le=1 count mismatch:\n%s", body)
	}
	if !strings.Contains(body, `llm_latency_seconds_bucket{model="m",provider="openai",le="0.5"} 0`) {
		t.Errorf("le=0.5 should be 0:\n%s", body)
	}
	if !strings.Contains(body, `llm_latency_seconds_bucket{model="m",provider="openai",le="+Inf"} 2`) {
		t.Errorf("+Inf bucket missing:\n%s", body)
	}
}

func TestTTFTHistogramOnlyFromStreaming(t *testing.T) {
	// Non-streaming records should NOT contribute to llm_ttft_seconds
	// even if their TTFT is set (defensive).
	rec := metrics.NewRecorder()
	now := time.Now()
	rec.Add(metrics.Record{
		Start: now, End: now.Add(100 * time.Millisecond),
		TTFT: 100 * time.Millisecond, Total: 100 * time.Millisecond,
		Model: "m", Provider: "openai", Status: 200, Stream: false,
	})
	body := scrapeBody(t, Handler(rec, StaticSource(nil)))
	if !strings.Contains(body, "# TYPE llm_ttft_seconds histogram") {
		t.Errorf("ttft family header missing:\n%s", body)
	}
	// The actual ttft counts should be empty (the family header is there
	// but no per-bucket lines).
	if strings.Contains(body, "llm_ttft_seconds_bucket{") {
		t.Errorf("non-streaming record leaked into TTFT histogram:\n%s", body)
	}
}

func TestAnomaliesChannelSource(t *testing.T) {
	rec := metrics.NewRecorder()
	ch := make(chan anomaly.Anomaly, 8)
	src := ChanSource(ch)

	// Push two of one kind, one of another.
	ch <- anomaly.Anomaly{Kind: anomaly.KindLatencySpike, Severity: anomaly.SeverityAlert}
	ch <- anomaly.Anomaly{Kind: anomaly.KindLatencySpike, Severity: anomaly.SeverityAlert}
	ch <- anomaly.Anomaly{Kind: anomaly.KindRepeatedPrompt, Severity: anomaly.SeverityInfo}

	body := scrapeBody(t, Handler(rec, src))

	if !strings.Contains(body, `llm_anomalies_total{kind="latency_spike",severity="alert"} 2`) {
		t.Errorf("latency_spike count wrong:\n%s", body)
	}
	if !strings.Contains(body, `llm_anomalies_total{kind="repeated_prompt",severity="info"} 1`) {
		t.Errorf("repeated_prompt count wrong:\n%s", body)
	}

	// Re-snapshot is stable (no double-counting).
	body2 := scrapeBody(t, Handler(rec, src))
	if !strings.Contains(body2, `llm_anomalies_total{kind="latency_spike",severity="alert"} 2`) {
		t.Errorf("re-snapshot double-counted:\n%s", body2)
	}
}

func TestChanSourceConcurrent(t *testing.T) {
	// Hammer the channel source from many goroutines. The drain must
	// be safe and final counts must match the total pushed.
	ch := make(chan anomaly.Anomaly, 1024)
	src := ChanSource(ch)

	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch <- anomaly.Anomaly{Kind: anomaly.KindTTFTSpike, Severity: anomaly.SeverityWarn}
		}()
	}
	wg.Wait()
	snap := src.Snapshot()
	if got := snap[AnomalyKey{Kind: anomaly.KindTTFTSpike, Severity: anomaly.SeverityWarn}]; got != N {
		t.Fatalf("chan source: want %d, got %d", N, got)
	}
}

func TestErrorKindBucketing(t *testing.T) {
	cases := []struct {
		status int
		err    string
		want   string
	}{
		{200, "", "other"}, // not an error path; classification irrelevant
		{401, "unauthorized", "4xx"},
		{408, "request timeout", "timeout"},
		{429, "rate limited", "4xx"},
		{500, "internal", "5xx"},
		{502, "upstream", "upstream"},
		{500, "", "5xx"},
		{0, "connection refused", "upstream"},
		{0, "i/o timeout", "timeout"},
		{0, "context deadline exceeded", "timeout"},
		{0, "build upstream: bad url", "internal"},
		{0, "weird exotic failure", "other"},
	}
	for _, c := range cases {
		got := errorKind(c.status, c.err)
		if got != c.want {
			t.Errorf("errorKind(%d, %q) = %q, want %q", c.status, c.err, got, c.want)
		}
	}
}

func TestEmptyRecorderEmitsWellFormedOutput(t *testing.T) {
	rec := metrics.NewRecorder()
	body := scrapeBody(t, Handler(rec, StaticSource(nil)))
	want := []string{
		"# HELP llm_requests_total",
		"# TYPE llm_requests_total counter",
		"# HELP llm_ttft_seconds",
		"# TYPE llm_ttft_seconds histogram",
		"# HELP go_goroutines",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("empty scrape missing %q\nbody:\n%s", w, body)
		}
	}
}

func TestFormatFloat(t *testing.T) {
	cases := map[float64]string{
		0.005: "0.005",
		0.5:   "0.5",
		1:     "1",
		2.5:   "2.5",
		10:    "10",
		0.025: "0.025",
	}
	for in, want := range cases {
		if got := formatFloat(in); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", in, got, want)
		}
	}
}

// scrapeBody runs the handler in an httptest server and returns the
// response body as a string. Fails the test on transport error.
func scrapeBody(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return readAll(t, resp.Body)
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}
