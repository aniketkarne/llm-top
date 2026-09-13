// Renderers for []Anomaly. Both renderers are pure: they take a
// writer and a slice and write nothing else. Determinism is the
// primary requirement so render_test.go can use snapshot-style
// golden strings.
package anomaly

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// RenderText writes a human-readable anomaly report to w. Format:
//
//	⚠ 3 anomalies
//	─────────────────────────────
//	WARN  ttft_spike       ttft 5,200ms on gpt-5.5 (baseline 420ms, x12.4)
//	WARN  latency_spike    total 12,000ms on claude-sonnet-5 (baseline 3,200ms, x3.8)
//	INFO  token_explosion  8,400 output tokens on gpt-5.5-mini (baseline 730, x11.5)
//	ALERT error_burst      75% errors on gpt-5.5/openai in last 20 requests (15/20)
//
// Empty input renders a single line so callers can blindly print.
func RenderText(w io.Writer, anomalies []Anomaly) {
	if len(anomalies) == 0 {
		fmt.Fprintln(w, "(no anomalies)")
		return
	}
	fmt.Fprintf(w, "⚠ %d anomalies\n", len(anomalies))
	// Divider width scales with the longest message, capped so the
	// output stays compact on terminals.
	maxLen := 30
	for _, a := range anomalies {
		if n := len(a.Message); n > maxLen {
			maxLen = n
		}
	}
	if maxLen > 80 {
		maxLen = 80
	}
	fmt.Fprintln(w, strings.Repeat("─", maxLen))
	for _, a := range anomalies {
		sev := strings.ToUpper(string(a.Severity))
		if sev == "" {
			sev = "INFO"
		}
		kind := string(a.Kind)
		if kind == "" {
			kind = "?"
		}
		fmt.Fprintf(w, "%-6s %-18s %s\n", sev, kind, a.Message)
	}
}

// RenderJSON writes the anomalies as a JSON array. Each element is
// the Anomaly struct verbatim (including all fields). Suitable for
// piping into jq or a log aggregator.
func RenderJSON(w io.Writer, anomalies []Anomaly) error {
	if anomalies == nil {
		anomalies = []Anomaly{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(anomalies)
}