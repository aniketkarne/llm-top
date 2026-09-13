package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aniketkarne-com/llm-top/internal/anomaly"
)

func TestAnomalyBadge(t *testing.T) {
	cases := []struct {
		kind anomaly.Kind
		want string
	}{
		{anomaly.KindTTFTSpike, "⚠ ttft_spike"},
		{anomaly.KindLatencySpike, "⚠ latency_spike"},
		{anomaly.KindTokenExplosion, "⚠ token_explosion"},
		{anomaly.KindRepeatedPrompt, "⚠ repeated_prompt"},
		{anomaly.KindErrorBurst, "⚠ error_burst"},
		{anomaly.KindLargeContext, "⚠ large_context"},
		{anomaly.KindProviderDegrade, "⚠ provider_degrade"},
		{anomaly.Kind("custom_thing"), "⚠ custom_thing"},
		{anomaly.Kind(""), "⚠ anomaly"},
	}
	for _, c := range cases {
		got := anomalyBadge(c.kind)
		if got != c.want {
			t.Errorf("anomalyBadge(%q) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestParseAnomaliesFromMetrics(t *testing.T) {
	body := []byte(`# HELP llm_anomalies_total ...
# TYPE llm_anomalies_total counter
llm_anomalies_total{kind="ttft_spike",severity="warn"} 3
llm_anomalies_total{kind="latency_spike",severity="warn"} 1
llm_anomalies_total{kind="error_burst",severity="alert"} 0
llm_requests_total{model="gpt-5.5",provider="openai",status="all"} 12
llm_errors_total{model="gpt-5.5",provider="openai",kind="5xx"} 1
`)
	got := parseAnomaliesFromMetrics(body)
	if got != 4 {
		t.Fatalf("parseAnomaliesFromMetrics sum = %d, want 4", got)
	}
}

func TestParseAnomaliesFromMetricsEmpty(t *testing.T) {
	if n := parseAnomaliesFromMetrics(nil); n != 0 {
		t.Fatalf("nil body: got %d, want 0", n)
	}
	if n := parseAnomaliesFromMetrics([]byte("")); n != 0 {
		t.Fatalf("empty body: got %d, want 0", n)
	}
}

func TestParseAnomaliesFromMetricsIgnoresGarbage(t *testing.T) {
	body := []byte(`llm_anomalies_total{kind="x",severity="warn"} NaN
llm_anomalies_total{kind="x",severity="warn"} -1
llm_anomalies_total{kind="x",severity="warn"} 2
llm_anomalies_total{kind="y"
not a real line
`)
	got := parseAnomaliesFromMetrics(body)
	if got != 2 {
		t.Fatalf("garbage-mixed body: got %d, want 2", got)
	}
}

func TestParseAnomaliesFromMetricsLarge(t *testing.T) {
	// Synthesize a body with >1000 line entries to exercise the
	// scanner buffer cap. Only the first two should be summed; the
	// rest have non-numeric values the parser skips.
	var b strings.Builder
	b.WriteString("llm_anomalies_total{kind=\"a\",severity=\"warn\"} 7\n")
	b.WriteString("llm_anomalies_total{kind=\"b\",severity=\"warn\"} 5\n")
	// Pad with junk to push past default buffer cap.
	for i := 0; i < 5000; i++ {
		b.WriteString("# padding comment line to consume buffer\n")
	}
	b.WriteString("llm_anomalies_total{kind=\"c\",severity=\"alert\"} 2\n")
	got := parseAnomaliesFromMetrics([]byte(b.String()))
	if got != 14 {
		t.Fatalf("large body: got %d, want 14", got)
	}
}

func TestStaticAnomalySource(t *testing.T) {
	src := &StaticAnomalySource{
		N: 3,
		ByID: map[string][]anomaly.Kind{
			"req-1": {anomaly.KindTTFTSpike},
			"req-2": {anomaly.KindLatencySpike, anomaly.KindErrorBurst},
		},
	}
	if src.Count() != 3 {
		t.Fatalf("Count: got %d, want 3", src.Count())
	}
	got := src.KindsForRequest("req-1")
	if len(got) != 1 || got[0] != anomaly.KindTTFTSpike {
		t.Fatalf("KindsForRequest req-1: got %v", got)
	}
	if src.KindsForRequest("missing") != nil {
		t.Fatalf("KindsForRequest missing: got %v, want nil", src.KindsForRequest("missing"))
	}
	// Nil receiver must not panic.
	var nilSrc *StaticAnomalySource
	if nilSrc.Count() != 0 {
		t.Fatal("nil Count should be 0")
	}
	if nilSrc.KindsForRequest("x") != nil {
		t.Fatal("nil KindsForRequest should be nil")
	}
}

func TestHTTPAnomalySource(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(`llm_anomalies_total{kind="ttft_spike",severity="warn"} 2
llm_anomalies_total{kind="error_burst",severity="alert"} 1
`))
	}))
	defer srv.Close()

	src := NewHTTPAnomalySource(srv.URL+"/metrics", nil)
	got := src.Count()
	if got != 3 {
		t.Fatalf("HTTPAnomalySource.Count: got %d, want 3", got)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", hits)
	}
	// No lookup configured; KindsForRequest should still be safe and
	// return nil.
	if src.KindsForRequest("req-1") != nil {
		t.Fatalf("KindsForRequest without lookup: got %v", src.KindsForRequest("req-1"))
	}
}

func TestHTTPAnomalySourceErrorTolerated(t *testing.T) {
	// Server that returns 500 — Count must swallow and return 0
	// rather than panicking.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	src := NewHTTPAnomalySource(srv.URL+"/metrics", nil)
	if got := src.Count(); got != 0 {
		t.Fatalf("Count on 500: got %d, want 0", got)
	}
}

func TestHTTPAnomalySourceUnreachableTolerated(t *testing.T) {
	// URL that will not resolve — Count must swallow and return 0.
	src := NewHTTPAnomalySource("http://127.0.0.1:1/never-listening", nil)
	if got := src.Count(); got != 0 {
		t.Fatalf("Count on unreachable: got %d, want 0", got)
	}
}

func TestHTTPAnomalySourceWithLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`llm_anomalies_total{kind="ttft_spike",severity="warn"} 1
`))
	}))
	defer srv.Close()
	lookup := func(id string) []anomaly.Kind {
		if id == "req-1" {
			return []anomaly.Kind{anomaly.KindTTFTSpike}
		}
		return nil
	}
	src := NewHTTPAnomalySource(srv.URL+"/metrics", lookup)
	if got := src.KindsForRequest("req-1"); len(got) != 1 {
		t.Fatalf("KindsForRequest with lookup: got %v", got)
	}
	if got := src.KindsForRequest("missing"); got != nil {
		t.Fatalf("KindsForRequest miss: got %v, want nil", got)
	}
}

func TestBannerText(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, ""},
		{1, " ⚠ 1 anomaly "},
		{2, " ⚠ 2 anomalies "},
		{42, " ⚠ 42 anomalies "},
	}
	for _, c := range cases {
		if got := bannerText(c.n); got != c.want {
			t.Errorf("bannerText(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
