package session

import (
	"math"
	"sort"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// AggregateStats summarizes a slice of captured requests. All fields are
// zero when the input is empty.
//
// Latency fields (TTFTMillis, TotalMillis) report zero for the
// percentiles when no requests in the slice reported that metric —
// callers should treat 0 as "not applicable". Errors carry the count
// of requests whose proxy.Request.Error is non-empty OR whose
// StatusCode is outside 2xx.
type AggregateStats struct {
	Count              int
	ErrorCount         int
	AvgTTFTMillis      float64
	P50TTFTMillis      float64
	P95TTFTMillis      float64
	AvgTotalMillis     float64
	P50TotalMillis     float64
	P95TotalMillis     float64
	TotalInputTokens   int
	TotalOutputTokens  int
	TotalCostUSD       float64
}

// Aggregate computes AggregateStats over the given requests. It is a
// pure function: same input always produces same output. Empty input
// returns the zero value of AggregateStats (Count == 0, ErrorCount == 0).
//
// The streaming-only metrics (TTFT) ignore requests that did not
// stream — those are detected via proxy.Request.Stream. Non-streaming
// requests contribute to Count, ErrorCount, token totals, cost, and
// TotalMillis percentiles.
func Aggregate(requests []proxy.Request) AggregateStats {
	if len(requests) == 0 {
		return AggregateStats{}
	}
	var (
		stats   AggregateStats
		ttfts   []float64
		totals  []float64
		inTok   int
		outTok  int
		errs    int
	)
	stats.Count = len(requests)
	for _, r := range requests {
		if isError(r) {
			errs++
		}
		totals = append(totals, float64(r.TotalMillis))
		if r.Stream && r.TTFTMillis > 0 {
			ttfts = append(ttfts, float64(r.TTFTMillis))
		}
		inTok += r.PromptTokens
		outTok += r.OutputTokens
		stats.TotalCostUSD += r.CostUSD
	}
	stats.ErrorCount = errs
	stats.TotalInputTokens = inTok
	stats.TotalOutputTokens = outTok
	if n := len(totals); n > 0 {
		stats.AvgTotalMillis = mean(totals)
		stats.P50TotalMillis = percentile(totals, 0.50)
		stats.P95TotalMillis = percentile(totals, 0.95)
	}
	if n := len(ttfts); n > 0 {
		stats.AvgTTFTMillis = mean(ttfts)
		stats.P50TTFTMillis = percentile(ttfts, 0.50)
		stats.P95TTFTMillis = percentile(ttfts, 0.95)
	}
	return stats
}

// DeltaStats captures absolute and percent change for every metric in
// AggregateStats. The "A" and "B" fields hold raw values from the two
// sessions so the renderer can show both sides; the "Delta" and "Pct"
// fields describe the change from A to B.
//
// Percent change is computed as (B - A) / A and returns -1 when A is 0
// (matching the "N/A" convention used in the user's mock — see
// render_test.go). For counts and integer metrics the raw fields are
// ints; for latencies and cost they are floats.
type DeltaStats struct {
	Count                [2]int
	CountPct             float64
	ErrorCount           [2]int
	ErrorCountDelta      int
	AvgTTFT              [2]float64
	AvgTTFTPct           float64
	AvgTTFTDelta         float64
	P50TTFT              [2]float64
	P50TTFTPct           float64
	P50TTFTDelta         float64
	P95TTFT              [2]float64
	P95TTFTPct           float64
	P95TTFTDelta         float64
	AvgTotal             [2]float64
	AvgTotalPct          float64
	AvgTotalDelta        float64
	P50Total             [2]float64
	P50TotalPct          float64
	P50TotalDelta        float64
	P95Total             [2]float64
	P95TotalPct          float64
	P95TotalDelta        float64
	InputTokens          [2]int
	InputTokensPct       float64
	InputTokensDelta     int
	OutputTokens         [2]int
	OutputTokensPct      float64
	OutputTokensDelta    int
	Cost                 [2]float64
	CostPct              float64
	CostDelta            float64
	CostPerRequest       [2]float64
	CostPerRequestPct    float64
	CostPerRequestDelta  float64
}

// Delta returns the absolute and percent change between two
// AggregateStats. The returned struct is laid out so the renderer can
// treat each metric identically: a [2]T pair holding the raw values
// from A and B, a Delta (B - A), and a Pct ((B - A) / A). The zero
// base value yields Pct = -1 (a "N/A" sentinel).
//
// "A" is the baseline (e.g. last release). "B" is what you are
// comparing against (e.g. current run).
func (a AggregateStats) Delta(b AggregateStats) DeltaStats {
	return DeltaStats{
		Count:               [2]int{a.Count, b.Count},
		CountPct:            pctDeltaInt(a.Count, b.Count),
		ErrorCount:          [2]int{a.ErrorCount, b.ErrorCount},
		ErrorCountDelta:     b.ErrorCount - a.ErrorCount,
		AvgTTFT:             [2]float64{a.AvgTTFTMillis, b.AvgTTFTMillis},
		AvgTTFTPct:          pctDeltaFloat(a.AvgTTFTMillis, b.AvgTTFTMillis),
		AvgTTFTDelta:        b.AvgTTFTMillis - a.AvgTTFTMillis,
		P50TTFT:             [2]float64{a.P50TTFTMillis, b.P50TTFTMillis},
		P50TTFTPct:          pctDeltaFloat(a.P50TTFTMillis, b.P50TTFTMillis),
		P50TTFTDelta:        b.P50TTFTMillis - a.P50TTFTMillis,
		P95TTFT:             [2]float64{a.P95TTFTMillis, b.P95TTFTMillis},
		P95TTFTPct:          pctDeltaFloat(a.P95TTFTMillis, b.P95TTFTMillis),
		P95TTFTDelta:        b.P95TTFTMillis - a.P95TTFTMillis,
		AvgTotal:            [2]float64{a.AvgTotalMillis, b.AvgTotalMillis},
		AvgTotalPct:         pctDeltaFloat(a.AvgTotalMillis, b.AvgTotalMillis),
		AvgTotalDelta:       b.AvgTotalMillis - a.AvgTotalMillis,
		P50Total:            [2]float64{a.P50TotalMillis, b.P50TotalMillis},
		P50TotalPct:         pctDeltaFloat(a.P50TotalMillis, b.P50TotalMillis),
		P50TotalDelta:       b.P50TotalMillis - a.P50TotalMillis,
		P95Total:            [2]float64{a.P95TotalMillis, b.P95TotalMillis},
		P95TotalPct:         pctDeltaFloat(a.P95TotalMillis, b.P95TotalMillis),
		P95TotalDelta:       b.P95TotalMillis - a.P95TotalMillis,
		InputTokens:         [2]int{a.TotalInputTokens, b.TotalInputTokens},
		InputTokensPct:      pctDeltaInt(a.TotalInputTokens, b.TotalInputTokens),
		InputTokensDelta:    b.TotalInputTokens - a.TotalInputTokens,
		OutputTokens:        [2]int{a.TotalOutputTokens, b.TotalOutputTokens},
		OutputTokensPct:     pctDeltaInt(a.TotalOutputTokens, b.TotalOutputTokens),
		OutputTokensDelta:   b.TotalOutputTokens - a.TotalOutputTokens,
		Cost:                [2]float64{a.TotalCostUSD, b.TotalCostUSD},
		CostPct:             pctDeltaFloat(a.TotalCostUSD, b.TotalCostUSD),
		CostDelta:           b.TotalCostUSD - a.TotalCostUSD,
		CostPerRequest:      [2]float64{costPerRequest(a), costPerRequest(b)},
		CostPerRequestPct:   pctDeltaFloat(costPerRequest(a), costPerRequest(b)),
		CostPerRequestDelta: costPerRequest(b) - costPerRequest(a),
	}
}

// isError returns true if the request counts as a failure for
// aggregation purposes. We treat any non-2xx status OR a transport
// error message as a failure. Zero status with no error message
// (e.g. an in-flight cancelled request that was never finalized)
// does NOT count as an error — those rows are rare in practice and
// bucketing them as failures would inflate error rates.
func isError(r proxy.Request) bool {
	if r.Error != "" {
		return true
	}
	if r.StatusCode != 0 && (r.StatusCode < 200 || r.StatusCode >= 300) {
		return true
	}
	return false
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// percentile returns the p-th percentile of xs (0 <= p <= 1) using
// the nearest-rank method. Returns 0 when xs is empty. The slice is
// sorted internally and not mutated.
//
// For p=0.95 on [1,2,3,...,100] this returns 95. For p=0.50 on the
// same input it returns 50 (not 50.5) — we prefer predictable
// integer-ish values for snapshot tests.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	// Nearest-rank: rank = ceil(p * N), clamped to [1, N].
	rank := int(math.Ceil(p * float64(len(cp))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(cp) {
		rank = len(cp)
	}
	return cp[rank-1]
}

// pctDeltaInt computes (b - a) / a as a float. Returns -1 when a is 0
// (a sentinel that the renderer treats as "N/A").
func pctDeltaInt(a, b int) float64 {
	if a == 0 {
		return -1
	}
	return float64(b-a) / float64(a)
}

// pctDeltaFloat is the float sibling of pctDeltaInt. -1 when base == 0.
func pctDeltaFloat(a, b float64) float64 {
	if a == 0 {
		return -1
	}
	return (b - a) / a
}

// costPerRequest returns total cost divided by request count, or 0
// when the session is empty.
func costPerRequest(s AggregateStats) float64 {
	if s.Count == 0 {
		return 0
	}
	return s.TotalCostUSD / float64(s.Count)
}