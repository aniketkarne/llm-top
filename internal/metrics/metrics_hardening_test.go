package metrics

import (
	"testing"
	"time"
)

// --- Hardening: edge cases ---

func TestEstimateTokensEmpty(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("EstimateTokens(\"\"): want 0, got %d", got)
	}
}

func TestEstimateTokensUnicode(t *testing.T) {
	// 16 emoji = 16 runes -> ceil(16/4) = 4 tokens
	got := EstimateTokens("🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀")
	if got != 4 {
		t.Errorf("EstimateTokens(16 emoji): want 4, got %d", got)
	}
}

func TestEstimateTokensRounding(t *testing.T) {
	// 5 runes -> ceil(5/4) = 2 tokens
	got := EstimateTokens("hello")
	if got != 2 {
		t.Errorf("EstimateTokens(\"hello\"): want 2, got %d", got)
	}
	// 4 runes -> 1 token
	got = EstimateTokens("hllo")
	if got != 1 {
		t.Errorf("EstimateTokens(\"hllo\"): want 1, got %d", got)
	}
}

func TestRecorderEmptySummary(t *testing.T) {
	r := NewRecorder()
	s := r.Summarize()
	if s.Count != 0 {
		t.Errorf("empty summary count: want 0, got %d", s.Count)
	}
	if s.AvgTTFT != 0 || s.AvgTotal != 0 {
		t.Errorf("empty summary should have zero durations")
	}
	if s.P50Total != 0 || s.P95Total != 0 || s.P99Total != 0 {
		t.Errorf("empty summary percentiles should be zero")
	}
}

func TestRecorderSingleRecord(t *testing.T) {
	r := NewRecorder()
	r.Add(Record{
		Start:     time.Now().Add(-100 * time.Millisecond),
		End:       time.Now(),
		TTFT:      50 * time.Millisecond,
		Total:     100 * time.Millisecond,
		PromptTok: 10,
		OutputTok: 20,
		Model:     "gpt-4o",
		Status:    200,
		Path:      "/v1/chat/completions",
		Stream:    true,
	})
	s := r.Summarize()
	if s.Count != 1 {
		t.Errorf("count: want 1, got %d", s.Count)
	}
	if s.TotalInTok != 10 {
		t.Errorf("TotalInTok: want 10, got %d", s.TotalInTok)
	}
	if s.TotalOutTok != 20 {
		t.Errorf("TotalOutTok: want 20, got %d", s.TotalOutTok)
	}
	if s.AvgTTFT != 50*time.Millisecond {
		t.Errorf("AvgTTFT: want 50ms, got %s", s.AvgTTFT)
	}
	if s.P50Total != 100*time.Millisecond {
		t.Errorf("P50Total: want 100ms, got %s", s.P50Total)
	}
}

func TestRecorderErrorCount(t *testing.T) {
	r := NewRecorder()
	r.Add(Record{Start: time.Now(), End: time.Now(), Total: time.Millisecond, Status: 200})
	r.Add(Record{Start: time.Now(), End: time.Now(), Total: time.Millisecond, Status: 500})
	r.Add(Record{Start: time.Now(), End: time.Now(), Total: time.Millisecond, Err: "boom"})
	r.Add(Record{Start: time.Now(), End: time.Now(), Total: time.Millisecond, Status: 200})
	s := r.Summarize()
	if s.Errors != 2 {
		t.Errorf("Errors: want 2 (500 status + Err msg), got %d", s.Errors)
	}
}

func TestRecorderSnapshotIndependent(t *testing.T) {
	// Modifying the snapshot must not affect future snapshots.
	r := NewRecorder()
	r.Add(Record{Total: time.Millisecond})
	snap1 := r.Snapshot()
	snap1[0].Total = time.Hour
	snap2 := r.Snapshot()
	if snap2[0].Total != time.Millisecond {
		t.Errorf("snapshot leak: snap2 has modified total = %s", snap2[0].Total)
	}
}

func TestPctile(t *testing.T) {
	// Sorted slice of 11 items -> indices 0..10.
	sorted := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
		5 * time.Millisecond,
		6 * time.Millisecond,
		7 * time.Millisecond,
		8 * time.Millisecond,
		9 * time.Millisecond,
		10 * time.Millisecond,
		11 * time.Millisecond,
	}
	if got := pctile(sorted, 0.50); got != 6*time.Millisecond {
		t.Errorf("p50 of 1..11ms: want 6ms, got %s", got)
	}
	if got := pctile(sorted, 0.0); got != 1*time.Millisecond {
		t.Errorf("p0: want 1ms, got %s", got)
	}
	if got := pctile(sorted, 1.0); got != 11*time.Millisecond {
		t.Errorf("p100: want 11ms, got %s", got)
	}
	// p95 -> index 9.5 -> linear interpolation between 10ms and 11ms -> 10.5ms
	// Allow tolerance for float arithmetic.
	got := pctile(sorted, 0.95)
	want := 10*time.Millisecond + (time.Millisecond)/2
	diff := got - want
	if diff < -time.Microsecond || diff > time.Microsecond {
		t.Errorf("p95 of 1..11ms: want %s (±1µs), got %s", want, got)
	}
}

func TestPctileEmpty(t *testing.T) {
	if got := pctile(nil, 0.5); got != 0 {
		t.Errorf("pctile on nil: want 0, got %s", got)
	}
	if got := pctile([]time.Duration{}, 0.5); got != 0 {
		t.Errorf("pctile on empty: want 0, got %s", got)
	}
}