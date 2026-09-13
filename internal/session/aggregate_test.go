package session

import (
	"math"
	"testing"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// mkReq constructs a proxy.Request with the given timing fields. We
// keep the construction here so test cases stay compact and the
// assertions focus on aggregation behavior, not data plumbing.
func mkReq(id string, stream bool, ttft, total int64, in, out int, cost float64, status int, errMsg string) proxy.Request {
	started := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	return proxy.Request{
		ID:           id,
		StartedAt:    started,
		EndedAt:      started.Add(time.Duration(total) * time.Millisecond),
		Method:       "POST",
		Path:         "/v1/chat/completions",
		Model:        "gpt-5.5-mini",
		Stream:       stream,
		StatusCode:   status,
		Error:        errMsg,
		TTFTMillis:   ttft,
		TotalMillis:  total,
		PromptTokens: in,
		OutputTokens: out,
		CostUSD:      cost,
	}
}

func TestAggregateEmpty(t *testing.T) {
	got := Aggregate(nil)
	if got.Count != 0 || got.ErrorCount != 0 || got.TotalCostUSD != 0 {
		t.Errorf("empty input should give zero stats, got %+v", got)
	}
	// Same with an empty (non-nil) slice — the function should not
	// panic or pretend to see data.
	got = Aggregate([]proxy.Request{})
	if got.Count != 0 {
		t.Errorf("empty slice: want Count=0, got %d", got.Count)
	}
}

func TestAggregateCountsAndErrors(t *testing.T) {
	requests := []proxy.Request{
		mkReq("a", true, 100, 1000, 10, 20, 0.001, 200, ""),
		mkReq("b", false, 0, 500, 10, 20, 0.001, 200, ""),
		mkReq("c", true, 200, 2000, 10, 20, 0.001, 500, "upstream error"),
		mkReq("d", false, 0, 800, 10, 20, 0.001, 404, ""),
	}
	got := Aggregate(requests)
	if got.Count != 4 {
		t.Errorf("Count: want 4, got %d", got.Count)
	}
	// Two failures: c (5xx + error) and d (4xx). Both count.
	if got.ErrorCount != 2 {
		t.Errorf("ErrorCount: want 2, got %d", got.ErrorCount)
	}
	if got.TotalInputTokens != 40 {
		t.Errorf("TotalInputTokens: want 40, got %d", got.TotalInputTokens)
	}
	if got.TotalOutputTokens != 80 {
		t.Errorf("TotalOutputTokens: want 80, got %d", got.TotalOutputTokens)
	}
	wantCost := 0.004
	if math.Abs(got.TotalCostUSD-wantCost) > 1e-9 {
		t.Errorf("TotalCostUSD: want %v, got %v", wantCost, got.TotalCostUSD)
	}
}

func TestAggregateTTFTOnlyStreaming(t *testing.T) {
	// Only requests with Stream=true AND TTFTMillis>0 contribute to
	// TTFT percentiles. Non-streaming requests have TTFTMillis=0 by
	// convention and must be ignored for that metric.
	requests := []proxy.Request{
		mkReq("a", true, 100, 1000, 1, 1, 0, 200, ""),
		mkReq("b", true, 200, 2000, 1, 1, 0, 200, ""),
		mkReq("c", true, 400, 4000, 1, 1, 0, 200, ""),
		mkReq("d", false, 0, 500, 1, 1, 0, 200, ""), // non-streaming, ignored
		mkReq("e", true, 0, 3000, 1, 1, 0, 200, ""), // streaming but zero TTFT — also ignored
	}
	got := Aggregate(requests)
	if got.Count != 5 {
		t.Errorf("Count: want 5 (all requests), got %d", got.Count)
	}
	if got.AvgTTFTMillis != (100+200+400)/3.0 {
		t.Errorf("AvgTTFT: want %v, got %v", float64(100+200+400)/3, got.AvgTTFTMillis)
	}
	// Percentiles over [100, 200, 400]: p50 = 200, p95 = 400.
	if got.P50TTFTMillis != 200 {
		t.Errorf("P50TTFT: want 200, got %v", got.P50TTFTMillis)
	}
	if got.P95TTFTMillis != 400 {
		t.Errorf("P95TTFT: want 400, got %v", got.P95TTFTMillis)
	}
}

func TestAggregateTotalIncludesAll(t *testing.T) {
	// TotalMillis percentiles include non-streaming requests too.
	requests := []proxy.Request{
		mkReq("a", true, 100, 1000, 1, 1, 0, 200, ""),
		mkReq("b", false, 0, 2000, 1, 1, 0, 200, ""),
		mkReq("c", true, 200, 3000, 1, 1, 0, 200, ""),
		mkReq("d", false, 0, 4000, 1, 1, 0, 200, ""),
	}
	got := Aggregate(requests)
	if got.AvgTotalMillis != (1000+2000+3000+4000)/4.0 {
		t.Errorf("AvgTotal: want 2500, got %v", got.AvgTotalMillis)
	}
	// p50 over [1000,2000,3000,4000] = 2000.
	if got.P50TotalMillis != 2000 {
		t.Errorf("P50Total: want 2000, got %v", got.P50TotalMillis)
	}
	// p95 = 4000.
	if got.P95TotalMillis != 4000 {
		t.Errorf("P95Total: want 4000, got %v", got.P95TotalMillis)
	}
}

func TestAggregateSingleRequest(t *testing.T) {
	// A single request should give mean == p50 == p95 for every metric.
	got := Aggregate([]proxy.Request{
		mkReq("only", true, 123, 456, 5, 7, 0.001, 200, ""),
	})
	if got.Count != 1 || got.ErrorCount != 0 {
		t.Errorf("count wrong: %+v", got)
	}
	if got.AvgTTFTMillis != 123 || got.P50TTFTMillis != 123 || got.P95TTFTMillis != 123 {
		t.Errorf("TTFT should all be 123: got %+v", got)
	}
	if got.AvgTotalMillis != 456 || got.P50TotalMillis != 456 || got.P95TotalMillis != 456 {
		t.Errorf("Total should all be 456: got %+v", got)
	}
}

func TestAggregateAllErrors(t *testing.T) {
	// Every request failed — should still compute totals normally;
	// ErrorCount == Count.
	requests := []proxy.Request{
		mkReq("a", false, 0, 100, 1, 0, 0, 502, "bad gateway"),
		mkReq("b", false, 0, 200, 1, 0, 0, 503, "service unavailable"),
	}
	got := Aggregate(requests)
	if got.ErrorCount != 2 || got.Count != 2 {
		t.Errorf("error counts: %+v", got)
	}
	if got.TotalInputTokens != 2 {
		t.Errorf("input tokens should still sum even on errors, got %d", got.TotalInputTokens)
	}
}

// --- Delta tests ---

func mkAgg(count, errCount int, avgTTFT, avgTotal, cost float64) AggregateStats {
	return AggregateStats{
		Count:           count,
		ErrorCount:      errCount,
		AvgTTFTMillis:   avgTTFT,
		AvgTotalMillis:  avgTotal,
		TotalCostUSD:    cost,
	}
}

func TestDeltaBasic(t *testing.T) {
	a := mkAgg(100, 5, 400, 1500, 5.00)
	b := mkAgg(110, 3, 350, 1400, 4.40)
	d := a.Delta(b)
	if d.Count != [2]int{100, 110} {
		t.Errorf("Count: got %v", d.Count)
	}
	if d.CountPct < 0.099 || d.CountPct > 0.101 {
		t.Errorf("CountPct: want 0.10, got %v", d.CountPct)
	}
	if d.ErrorCount != [2]int{5, 3} || d.ErrorCountDelta != -2 {
		t.Errorf("ErrorCount: %+v", d)
	}
	if d.AvgTTFTDelta != -50 {
		t.Errorf("AvgTTFTDelta: want -50, got %v", d.AvgTTFTDelta)
	}
	if math.Abs(d.CostDelta-(-0.60)) > 1e-9 {
		t.Errorf("CostDelta: want -0.60, got %v", d.CostDelta)
	}
}

func TestDeltaZeroBaseYieldsNegativeOne(t *testing.T) {
	// When the baseline is 0, percent is undefined. We use -1 as a
	// sentinel that the renderer recognizes as "N/A".
	a := mkAgg(0, 0, 0, 0, 0)
	b := mkAgg(5, 0, 100, 1000, 1.00)
	d := a.Delta(b)
	if d.CountPct != -1 {
		t.Errorf("CountPct on zero base: want -1, got %v", d.CountPct)
	}
	if d.AvgTTFTPct != -1 {
		t.Errorf("AvgTTFTPct on zero base: want -1, got %v", d.AvgTTFTPct)
	}
	if d.CostPct != -1 {
		t.Errorf("CostPct on zero base: want -1, got %v", d.CostPct)
	}
	// Absolute deltas should still report correctly even when pct is N/A.
	if d.Count != [2]int{0, 5} {
		t.Errorf("Count pair on zero base: %v", d.Count)
	}
	if d.CostDelta != 1.00 {
		t.Errorf("CostDelta on zero base: want 1.00, got %v", d.CostDelta)
	}
}

func TestDeltaCostPerRequest(t *testing.T) {
	// CostPerRequest = TotalCostUSD / Count. The user wants to see
	// "$0.081 → 0.057" in the diff, which is per-request.
	a := mkAgg(100, 0, 0, 0, 8.10)
	b := mkAgg(100, 0, 0, 0, 5.70)
	d := a.Delta(b)
	if d.CostPerRequest != [2]float64{0.081, 0.057} {
		t.Errorf("CostPerRequest: want [0.081, 0.057], got %v", d.CostPerRequest)
	}
	// Pct: (5.70 - 8.10) / 8.10 ≈ -0.296
	wantPct := -0.2962962962962963
	if math.Abs(d.CostPerRequestPct-wantPct) > 1e-9 {
		t.Errorf("CostPerRequestPct: want %v, got %v", wantPct, d.CostPerRequestPct)
	}
}

func TestDeltaCostPerRequestZeroCount(t *testing.T) {
	// When count is zero, costPerRequest returns 0. Comparing two
	// zeros gives a -1 pct (zero base) — fine. The important thing
	// is no panic and no NaN.
	a := mkAgg(0, 0, 0, 0, 0)
	b := mkAgg(0, 0, 0, 0, 0)
	d := a.Delta(b)
	if d.CostPerRequest != [2]float64{0, 0} {
		t.Errorf("CostPerRequest on zero count: %v", d.CostPerRequest)
	}
	if math.IsNaN(d.CostPerRequestPct) {
		t.Errorf("CostPerRequestPct must not be NaN")
	}
}

// --- Internal helper sanity checks ---

func TestPercentileNearestRank(t *testing.T) {
	cases := []struct {
		xs   []float64
		p    float64
		want float64
	}{
		{[]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.50, 5},
		{[]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.95, 10},
		{[]float64{1, 2, 3, 4, 5}, 0.50, 3},
		{[]float64{1}, 0.50, 1},
		{[]float64{}, 0.50, 0},
	}
	for _, c := range cases {
		got := percentile(c.xs, c.p)
		if got != c.want {
			t.Errorf("percentile(%v, %v): want %v, got %v", c.xs, c.p, c.want, got)
		}
	}
}

func TestPercentileDoesNotMutateInput(t *testing.T) {
	xs := []float64{3, 1, 4, 1, 5, 9, 2, 6}
	original := make([]float64, len(xs))
	copy(original, xs)
	percentile(xs, 0.95)
	for i := range xs {
		if xs[i] != original[i] {
			t.Errorf("percentile mutated input at %d: %v vs %v", i, xs[i], original[i])
		}
	}
}