package metrics

import (
	"testing"
	"time"
)

func TestRecorderAddAndSnapshot(t *testing.T) {
	r := NewRecorder()
	r.Add(Record{Status: 200, OutputTok: 10})
	r.Add(Record{Status: 500, Err: "boom"})
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len: want 2, got %d", len(snap))
	}
	if snap[0].OutputTok != 10 || snap[1].Status != 500 {
		t.Fatalf("snapshot contents wrong")
	}
}

func TestSummarizeEmpty(t *testing.T) {
	r := NewRecorder()
	s := r.Summarize()
	if s.Count != 0 {
		t.Fatalf("count: want 0, got %d", s.Count)
	}
}

func TestSummarizeAggregatesAndPercentiles(t *testing.T) {
	r := NewRecorder()
	for i, d := range []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
		40 * time.Millisecond,
		100 * time.Millisecond,
	} {
		r.Add(Record{
			Status:    200,
			Total:     d,
			OutputTok: 5 + i,
			PromptTok: 1,
		})
	}
	s := r.Summarize()
	if s.Count != 5 {
		t.Fatalf("count: want 5, got %d", s.Count)
	}
	wantOut := 5 + 6 + 7 + 8 + 9
	if s.TotalOutTok != wantOut {
		t.Fatalf("output tokens: want %d, got %d", wantOut, s.TotalOutTok)
	}
	if s.P50Total != 30*time.Millisecond {
		t.Fatalf("p50: want 30ms, got %v", s.P50Total)
	}
	// Linear-interpolated percentiles (NIST type 7).
	// p95 = 0.95 * 4 = 3.8 → sorted[3] + 0.8*(sorted[4]-sorted[3]) = 30ms + 0.8*70ms = 86ms
	if s.P95Total < 85*time.Millisecond || s.P95Total > 90*time.Millisecond {
		t.Fatalf("p95: want ~86ms, got %v", s.P95Total)
	}
	// p99 = 0.99 * 4 = 3.96 → sorted[3] + 0.96*(sorted[4]-sorted[3]) = 30ms + 0.96*70ms ≈ 97ms
	if s.P99Total < 95*time.Millisecond || s.P99Total > 100*time.Millisecond {
		t.Fatalf("p99: want ~97ms, got %v", s.P99Total)
	}
}

func TestSummarizeCountsErrors(t *testing.T) {
	r := NewRecorder()
	r.Add(Record{Status: 200})
	r.Add(Record{Status: 500, Err: "boom"})
	r.Add(Record{Status: 404})
	s := r.Summarize()
	if s.Errors != 2 {
		t.Fatalf("errors: want 2, got %d", s.Errors)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abcd", 1},
		{"abcde", 2}, // 5/4 -> (5+3)/4 = 2
		{"abcdefgh", 2},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q): want %d, got %d", c.in, c.want, got)
		}
	}
}
