// Package metrics records per-request latency, time-to-first-token (TTFT),
// and token-count estimates for an LLM proxy.
package metrics

import (
	"sort"
	"sync"
	"time"
)

// Record represents a single completed request.
type Record struct {
	Start     time.Time     `json:"start"`
	End       time.Time     `json:"end"`
	TTFT      time.Duration `json:"ttft"`  // time until first SSE data event
	Total     time.Duration `json:"total"` // total wall clock
	PromptTok int           `json:"prompt_tokens"`
	OutputTok int           `json:"output_tokens"`
	Model     string        `json:"model"`
	Status    int           `json:"status"` // HTTP status of upstream response
	Path      string        `json:"path"`
	Stream    bool          `json:"stream"`
	Err       string        `json:"error,omitempty"`
}

// Recorder is a thread-safe collector of Records.
type Recorder struct {
	mu      sync.Mutex
	records []Record
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Add inserts a completed record.
func (r *Recorder) Add(rec Record) {
	r.mu.Lock()
	r.records = append(r.records, rec)
	r.mu.Unlock()
}

// Snapshot returns a copy of all records (most recent last).
func (r *Recorder) Snapshot() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, len(r.records))
	copy(out, r.records)
	return out
}

// Summary aggregates the current records into summary statistics.
type Summary struct {
	Count       int           `json:"count"`
	TotalOutTok int           `json:"total_output_tokens"`
	TotalInTok  int           `json:"total_prompt_tokens"`
	AvgTTFT     time.Duration `json:"avg_ttft"`
	AvgTotal    time.Duration `json:"avg_total"`
	P50Total    time.Duration `json:"p50_total"`
	P95Total    time.Duration `json:"p95_total"`
	P99Total    time.Duration `json:"p99_total"`
	Errors      int           `json:"errors"`
}

// Summarize computes summary statistics over the recorder's records.
// If there are no records, returns the zero Summary.
func (r *Recorder) Summarize() Summary {
	recs := r.Snapshot()
	s := Summary{Count: len(recs)}
	if len(recs) == 0 {
		return s
	}
	totals := make([]time.Duration, 0, len(recs))
	for _, rec := range recs {
		s.TotalOutTok += rec.OutputTok
		s.TotalInTok += rec.PromptTok
		s.AvgTTFT += rec.TTFT
		s.AvgTotal += rec.Total
		totals = append(totals, rec.Total)
		if rec.Err != "" || rec.Status >= 400 {
			s.Errors++
		}
	}
	n := time.Duration(len(recs))
	s.AvgTTFT /= n
	s.AvgTotal /= n
	sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })
	s.P50Total = pctile(totals, 0.50)
	s.P95Total = pctile(totals, 0.95)
	s.P99Total = pctile(totals, 0.99)
	return s
}

func pctile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	// Linear-interpolated percentile (NIST type 7 / numpy default).
	pos := p * float64(len(sorted)-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	diff := sorted[hi] - sorted[lo]
	return sorted[lo] + time.Duration(float64(diff)*frac)
}

// EstimateTokens returns a conservative token estimate for a UTF-8 string.
// Roughly 1 token per 4 bytes (close to OpenAI's average for English).
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	// count runes for better international handling
	n := 0
	for range s {
		n++
	}
	return (n + 3) / 4
}
