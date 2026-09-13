package session

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// fixedTime is a deterministic UTC instant used for snapshot-style
// renders — keeps tests reproducible regardless of when they run.
var fixedTime = time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)

func TestRenderShowMinimal(t *testing.T) {
	var buf bytes.Buffer
	s := Session{
		ID:        "0123456789abcdef",
		StartedAt: fixedTime,
		EndedAt:   fixedTime.Add(5 * time.Minute),
		Active:    false,
	}
	agg := AggregateStats{Count: 3}
	RenderShow(&buf, s, agg)
	out := buf.String()
	// Header lines must be present.
	if !strings.Contains(out, "session: 0123456789abcdef") {
		t.Errorf("missing session id line: %s", out)
	}
	if !strings.Contains(out, "started: 2026-09-13 10:00:00 UTC") {
		t.Errorf("missing started: %s", out)
	}
	if !strings.Contains(out, "ended:") || strings.Contains(out, "ended:   active") {
		t.Errorf("expected concrete ended timestamp, got: %s", out)
	}
	if !strings.Contains(out, "requests:   3") {
		t.Errorf("missing requests count: %s", out)
	}
	if strings.Contains(out, "(no requests captured)") {
		t.Errorf("should not show 'no requests' fallback when Count > 0: %s", out)
	}
}

func TestRenderShowFull(t *testing.T) {
	// A populated session should render every metric row in order.
	var buf bytes.Buffer
	s := Session{
		ID:        "abc123",
		Name:      "baseline-2026-09-13",
		Label:     "perf",
		Tags:      []string{"release", "canary"},
		StartedAt: fixedTime,
		EndedAt:   fixedTime.Add(5 * time.Minute),
		Active:    false,
	}
	agg := AggregateStats{
		Count:             182,
		ErrorCount:        4,
		AvgTTFTMillis:     421,
		P50TTFTMillis:     380,
		P95TTFTMillis:     920,
		AvgTotalMillis:    1420,
		P50TotalMillis:    1200,
		P95TotalMillis:    2910,
		TotalInputTokens:  8421,
		TotalOutputTokens: 3142,
		TotalCostUSD:      14.74,
	}
	RenderShow(&buf, s, agg)
	out := buf.String()

	mustContain := []string{
		`name="baseline-2026-09-13"`,
		`label="perf"`,
		"tags:    release, canary",
		"started: 2026-09-13 10:00:00 UTC",
		"ended:   2026-09-13 10:05:00 UTC",
		"requests:   182",
		"errors:     4",
		"avg TTFT:   421ms",
		"avg total:  1.42s",
		"input:      8.4k tokens",
		"output:     3.1k tokens",
		"cost:       $14.74",
		"$0.081 per request", // 14.74/182 ≈ 0.0810
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("RenderShow missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderShowActiveSession(t *testing.T) {
	var buf bytes.Buffer
	s := Session{
		ID:        "live",
		StartedAt: fixedTime,
		// EndedAt zero -> active.
		Active: true,
	}
	RenderShow(&buf, s, AggregateStats{})
	if !strings.Contains(buf.String(), "ended:   active") {
		t.Errorf("active session should print 'ended: active': %s", buf.String())
	}
}

func TestRenderDiffExactMockFormat(t *testing.T) {
	// This is the exact format the user sketched in the plan. The
	// assertions are the spec; if you change the renderer, update
	// this test deliberately.
	var buf bytes.Buffer
	a := Session{ID: "aaaa", Label: "A"}
	b := Session{ID: "bbbb", Label: "B"}
	aAgg := AggregateStats{
		Count:             182,
		ErrorCount:        4,
		AvgTTFTMillis:     421,
		AvgTotalMillis:    1500,
		P95TotalMillis:    2910,
		TotalInputTokens:  8421,
		TotalOutputTokens: 3142,
		TotalCostUSD:      14.74,
	}
	bAgg := AggregateStats{
		Count:             182,
		ErrorCount:        1,
		AvgTTFTMillis:     308,
		AvgTotalMillis:    1300,
		P95TotalMillis:    2170,
		TotalInputTokens:  6100,
		TotalOutputTokens: 2900,
		TotalCostUSD:      10.40,
	}
	RenderDiff(&buf, a, aAgg, b, bAgg)

	want := "" +
		"Session A vs B\n" +
		"\n" +
		"requests:      182 → 182\n" +
		"avg TTFT:      421ms → 308ms\n" +
		"p95 latency:   2.91s → 2.17s\n" +
		"input tokens:  8.4k → 6.1k\n" +
		"output tokens: 3.1k → 2.9k\n" +
		"cost/request:  $0.081 → 0.057\n" +
		"total cost:    $14.74 → 10.40\n" +
		"errors:        4 → 1\n"
	if buf.String() != want {
		t.Errorf("diff output mismatch.\n--- want ---\n%s\n--- got ---\n%s", want, buf.String())
	}
}

func TestRenderDiffLabelFallback(t *testing.T) {
	// No label, no name → truncated id.
	var buf bytes.Buffer
	a := Session{ID: "abcdef1234567890"}
	b := Session{ID: "deadbeefcafebabe"}
	RenderDiff(&buf, a, AggregateStats{}, b, AggregateStats{})
	out := buf.String()
	if !strings.HasPrefix(out, "Session abcdef12 vs deadbeef\n") {
		t.Errorf("label fallback wrong: %s", out)
	}
}

func TestRenderDiffLabelNameCombination(t *testing.T) {
	var buf bytes.Buffer
	a := Session{ID: "aa", Label: "perf", Name: "baseline"}
	b := Session{ID: "bb", Label: "perf", Name: "current"}
	RenderDiff(&buf, a, AggregateStats{}, b, AggregateStats{})
	if !strings.HasPrefix(buf.String(), "Session perf/baseline vs perf/current\n") {
		t.Errorf("combined label/name: %s", buf.String())
	}
}

func TestRenderDiffEmptySessions(t *testing.T) {
	// Both sessions have no requests — output should still render
	// the header and the rows with zero values.
	var buf bytes.Buffer
	a := Session{ID: "aa", Label: "A"}
	b := Session{ID: "bb", Label: "B"}
	RenderDiff(&buf, a, AggregateStats{}, b, AggregateStats{})
	out := buf.String()
	if !strings.HasPrefix(out, "Session A vs B\n") {
		t.Errorf("header: %s", out)
	}
	if !strings.Contains(out, "requests:      0 → 0\n") {
		t.Errorf("zero counts: %s", out)
	}
	// p95 latency row uses "-" for zero.
	if !strings.Contains(out, "p95 latency:   - → -\n") {
		t.Errorf("zero p95: %s", out)
	}
	// Cost rows: left has $, right is bare.
	if !strings.Contains(out, "cost/request:  $0.00 → 0.00\n") {
		t.Errorf("zero cost/request row: %s", out)
	}
}

func TestHumanizeMillis(t *testing.T) {
	cases := []struct {
		ms   float64
		want string
	}{
		{0, "-"},
		{-1, "-"},
		{1, "1ms"},
		{999, "999ms"},
		{1000, "1.00s"},
		{1500, "1.50s"},
		{2910, "2.91s"},
		{12345, "12.35s"}, // rounded
	}
	for _, c := range cases {
		if got := HumanizeMillis(c.ms); got != c.want {
			t.Errorf("HumanizeMillis(%v): want %q, got %q", c.ms, c.want, got)
		}
	}
}

func TestHumanizeSeconds(t *testing.T) {
	cases := []struct {
		ms   float64
		want string
	}{
		{0, "-"},
		{2910, "2.91s"},
		{500, "0.50s"},
		{2170, "2.17s"},
	}
	for _, c := range cases {
		if got := HumanizeSeconds(c.ms); got != c.want {
			t.Errorf("HumanizeSeconds(%v): want %q, got %q", c.ms, c.want, got)
		}
	}
}

func TestHumanizeCount(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{8421, "8,421"},
		{1234567, "1,234,567"},
		{-42, "-42"},
		{-1234, "-1,234"},
	}
	for _, c := range cases {
		if got := HumanizeCount(c.n); got != c.want {
			t.Errorf("HumanizeCount(%d): want %q, got %q", c.n, c.want, got)
		}
	}
}

func TestHumanizeTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1.0k"},
		{8421, "8.4k"},
		{6100, "6.1k"}, // exactly the user's mock
		{3142, "3.1k"},
		{8420, "8.4k"}, // rounds to same bucket as 8421
		{1000000, "1000.0k"},
	}
	for _, c := range cases {
		if got := HumanizeTokens(c.n); got != c.want {
			t.Errorf("HumanizeTokens(%d): want %q, got %q", c.n, c.want, got)
		}
	}
}

func TestHumanizeCost(t *testing.T) {
	cases := []struct {
		usd  float64
		want string
	}{
		{0, "$0.00"},
		{0.081, "$0.081"},
		{0.057, "$0.057"},
		{1.00, "$1.00"},
		{14.74, "$14.74"},
		{0.004, "$0.004"},
	}
	for _, c := range cases {
		if got := HumanizeCost(c.usd); got != c.want {
			t.Errorf("HumanizeCost(%v): want %q, got %q", c.usd, c.want, got)
		}
	}
}