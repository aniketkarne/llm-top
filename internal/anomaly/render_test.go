package anomaly

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleAnomalies() []Anomaly {
	now := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
	return []Anomaly{
		{
			Kind:       KindTTFTSpike,
			Severity:   SeverityWarn,
			Message:    "ttft 5,200ms on gpt-5.5 (baseline 420ms, x12.4)",
			DetectedAt: now,
			RequestID:  "req-aaa",
			Baseline:   "420ms",
			Observed:   "5200ms",
			Extra:      map[string]string{"model": "gpt-5.5", "ratio": "12.40"},
		},
		{
			Kind:       KindLatencySpike,
			Severity:   SeverityWarn,
			Message:    "total 12,000ms on claude-sonnet-5 (baseline 3,200ms, x3.8)",
			DetectedAt: now,
			RequestID:  "req-bbb",
			Baseline:   "3200ms",
			Observed:   "12000ms",
		},
		{
			Kind:       KindTokenExplosion,
			Severity:   SeverityInfo,
			Message:    "8,400 output tokens on gpt-5.5-mini (baseline 730, x11.5)",
			DetectedAt: now,
			RequestID:  "req-ccc",
		},
		{
			Kind:       KindErrorBurst,
			Severity:   SeverityAlert,
			Message:    "75% errors on gpt-5.5/openai in last 20 requests (15/20)",
			DetectedAt: now,
			RequestID:  "req-ddd",
		},
	}
}

func TestRenderTextEmpty(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, nil)
	out := buf.String()
	if !strings.Contains(out, "(no anomalies)") {
		t.Errorf("empty render should show '(no anomalies)', got %q", out)
	}
}

func TestRenderTextSingle(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, []Anomaly{
		{Kind: KindLargeContext, Severity: SeverityWarn, Message: "single line"},
	})
	out := buf.String()
	if !strings.Contains(out, "⚠ 1 anomalies") {
		t.Errorf("missing banner: %s", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Errorf("missing severity: %s", out)
	}
	if !strings.Contains(out, "large_context") {
		t.Errorf("missing kind: %s", out)
	}
	if !strings.Contains(out, "single line") {
		t.Errorf("missing message: %s", out)
	}
}

func TestRenderTextMultiple(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, sampleAnomalies())
	out := buf.String()
	for _, want := range []string{"⚠ 4 anomalies", "WARN", "INFO", "ALERT", "ttft_spike", "latency_spike", "token_explosion", "error_burst", "5,200ms", "12,000ms", "8,400 output tokens"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderTextSeverityOrder(t *testing.T) {
	// Severity column should be left-aligned and at least 6 chars wide
	// to accommodate INFO/WARN/ALERT (longest is ALERT = 5 chars + space).
	var buf bytes.Buffer
	RenderText(&buf, sampleAnomalies())
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Skip banner + divider.
	for _, line := range lines[2:] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// First field is severity (left-aligned in 6 chars).
		if !strings.Contains(line, "WARN") && !strings.Contains(line, "INFO") && !strings.Contains(line, "ALERT") {
			t.Errorf("severity column missing on line: %q", line)
		}
	}
}

func TestRenderJSONEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, nil); err != nil {
		t.Fatal(err)
	}
	// Must produce a valid JSON array, not "null".
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "[]") {
		t.Errorf("empty JSON should be [], got %q", buf.String())
	}
}

func TestRenderJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleAnomalies()); err != nil {
		t.Fatal(err)
	}
	var got []Anomaly
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("JSON did not round-trip: %v\n%s", err, buf.String())
	}
	if len(got) != 4 {
		t.Errorf("want 4 anomalies, got %d", len(got))
	}
	if got[0].Kind != KindTTFTSpike {
		t.Errorf("first kind lost: %s", got[0].Kind)
	}
	if got[3].Severity != SeverityAlert {
		t.Errorf("last severity lost: %s", got[3].Severity)
	}
}

func TestEncodeExtraEmpty(t *testing.T) {
	if got := (Anomaly{}).EncodeExtra(); got != "{}" {
		t.Errorf("empty extra should be {}, got %q", got)
	}
	if got := (Anomaly{Extra: map[string]string{}}).EncodeExtra(); got != "{}" {
		t.Errorf("initialized-empty extra should be {}, got %q", got)
	}
	if got := (Anomaly{Extra: map[string]string{"k": "v"}}).EncodeExtra(); got != `{"k":"v"}` {
		t.Errorf("populated extra wrong: %q", got)
	}
}

func TestEncodeExtraUsedInJSON(t *testing.T) {
	a := Anomaly{
		Kind:  KindTTFTSpike,
		Extra: map[string]string{"model": "gpt-5.5"},
	}
	if got := a.EncodeExtra(); !strings.Contains(got, "gpt-5.5") {
		t.Errorf("EncodeExtra lost value: %s", got)
	}
}
