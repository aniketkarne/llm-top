package session

import (
	"fmt"
	"io"
	"strings"
)

// RenderShow writes a human-readable summary of a single session and
// its aggregate stats to w. The output is plain text (no ANSI) and
// stable across runs so it is safe to put under snapshot tests.
//
// Layout:
//
//	session: <id>  name=<name>  label=<label>
//	tags:    t1, t2
//	started: 2026-09-13 10:00:00 UTC
//	ended:   2026-09-13 10:05:00 UTC   (or "active")
//
//	requests:    182
//	errors:        4
//	avg TTFT:   421ms     (p50 380ms, p95 920ms)
//	avg total: 1.42s      (p50 1.20s, p95 2.91s)
//	input:     8.2k tokens
//	output:    3.1k tokens
//	cost:      $14.74     ($0.081 per request)
//
// Empty fields are omitted from the metadata block.
func RenderShow(w io.Writer, s Session, a AggregateStats) {
	fmt.Fprintf(w, "session: %s", s.ID)
	if s.Name != "" {
		fmt.Fprintf(w, "  name=%q", s.Name)
	}
	if s.Label != "" {
		fmt.Fprintf(w, "  label=%q", s.Label)
	}
	fmt.Fprintln(w)

	if len(s.Tags) > 0 {
		fmt.Fprintf(w, "tags:    %s\n", strings.Join(s.Tags, ", "))
	}

	startedStr := s.StartedAt.UTC().Format("2006-01-02 15:04:05")
	if !s.StartedAt.IsZero() {
		fmt.Fprintf(w, "started: %s UTC\n", startedStr)
	}
	if !s.EndedAt.IsZero() {
		fmt.Fprintf(w, "ended:   %s UTC\n", s.EndedAt.UTC().Format("2006-01-02 15:04:05"))
	} else {
		fmt.Fprintln(w, "ended:   active")
	}

	fmt.Fprintln(w)
	if a.Count == 0 {
		fmt.Fprintln(w, "(no requests captured)")
		return
	}

	fmt.Fprintf(w, "requests:   %s\n", HumanizeCount(a.Count))
	fmt.Fprintf(w, "errors:     %s\n", HumanizeCount(a.ErrorCount))

	if a.P50TTFTMillis > 0 || a.AvgTTFTMillis > 0 || a.P95TTFTMillis > 0 {
		fmt.Fprintf(w, "avg TTFT:   %-8s (p50 %s, p95 %s)\n",
			HumanizeMillis(a.AvgTTFTMillis),
			HumanizeMillis(a.P50TTFTMillis),
			HumanizeMillis(a.P95TTFTMillis),
		)
	}
	fmt.Fprintf(w, "avg total:  %-8s (p50 %s, p95 %s)\n",
		HumanizeMillis(a.AvgTotalMillis),
		HumanizeMillis(a.P50TotalMillis),
		HumanizeMillis(a.P95TotalMillis),
	)

	if a.TotalInputTokens > 0 {
		fmt.Fprintf(w, "input:      %s tokens\n", HumanizeTokens(a.TotalInputTokens))
	}
	if a.TotalOutputTokens > 0 {
		fmt.Fprintf(w, "output:     %s tokens\n", HumanizeTokens(a.TotalOutputTokens))
	}
	if a.TotalCostUSD > 0 {
		fmt.Fprintf(w, "cost:       %s", HumanizeCost(a.TotalCostUSD))
		if a.Count > 0 {
			fmt.Fprintf(w, "     (%s per request)", HumanizeCost(a.TotalCostUSD/float64(a.Count)))
		}
		fmt.Fprintln(w)
	}
}

// RenderDiff writes the side-by-side delta between two sessions in
// the exact format the user sketched:
//
//	Session <labelA> vs <labelB>
//
//	requests:       182 → 182
//	avg TTFT:       421ms → 308ms
//	p95 latency:   2.91s → 2.17s
//	input tokens:  8.2k → 6.1k
//	cost/request:  $0.081 → 0.057
//	errors:           4 → 1
//
// "Label" falls back to a truncated session ID when name+label are
// both empty, so the header is never blank. The p95 row uses the
// P95 total latency (wall-clock), which is the most common debugging
// question; the TTFT row shows the streaming-relevant percentile.
//
// Cost rows are formatted asymmetrically: the left side carries the
// "$" prefix and the right side is bare, matching the user's mock
// ("$0.081 → 0.057"). This keeps the eye anchored on the currency
// once per row rather than twice.
func RenderDiff(w io.Writer, aSess Session, aAgg AggregateStats, bSess Session, bAgg AggregateStats) {
	headerA := labelFor(&aSess)
	headerB := labelFor(&bSess)
	fmt.Fprintf(w, "Session %s vs %s\n\n", headerA, headerB)

	row := func(label, va, vb string) {
		// The user's mock aligns every value column at offset 16:
		// "requests:" (9 chars) + 7 spaces + "182", or "errors:" (7
		// chars) + 9 spaces + "4". Achieved by padding the label
		// field to 15 chars and writing values flush against it.
		fmt.Fprintf(w, "%-15s%s → %s\n", label+":", va, vb)
	}

	// requests / errors first (most asked-about signals).
	row("requests", HumanizeCount(aAgg.Count), HumanizeCount(bAgg.Count))
	row("avg TTFT", HumanizeMillis(aAgg.AvgTTFTMillis), HumanizeMillis(bAgg.AvgTTFTMillis))
	// The user asked for "p95 latency" — interpret as wall-clock p95.
	row("p95 latency", HumanizeSeconds(aAgg.P95TotalMillis), HumanizeSeconds(bAgg.P95TotalMillis))
	row("input tokens", HumanizeTokens(aAgg.TotalInputTokens), HumanizeTokens(bAgg.TotalInputTokens))
	row("output tokens", HumanizeTokens(aAgg.TotalOutputTokens), HumanizeTokens(bAgg.TotalOutputTokens))
	cpaA := costPerRequest(aAgg)
	cpaB := costPerRequest(bAgg)
	row("cost/request", HumanizeCost(cpaA), HumanizeCostBare(cpaB))
	row("total cost", HumanizeCost(aAgg.TotalCostUSD), HumanizeCostBare(bAgg.TotalCostUSD))
	row("errors", HumanizeCount(aAgg.ErrorCount), HumanizeCount(bAgg.ErrorCount))
}

// labelFor picks the most readable identifier for a session for use
// in the diff header: label+name > label > name > truncated ID.
func labelFor(s *Session) string {
	if s.Label != "" && s.Name != "" {
		return s.Label + "/" + s.Name
	}
	if s.Label != "" {
		return s.Label
	}
	if s.Name != "" {
		return s.Name
	}
	if len(s.ID) >= 8 {
		return s.ID[:8]
	}
	return s.ID
}

// --- Humanize helpers ---
//
// All functions are deterministic and side-effect-free; the same input
// always produces the same output. They are public so render_test.go
// can lock the formatting down with snapshot tests, and so other
// packages (TUI in Step 7, future exporters) can reuse them.

// HumanizeMillis renders a millisecond duration as either "<n>ms" or
// "<n.n>s" depending on magnitude. < 1000 → ms; >= 1000 → seconds
// with two decimals (so 1500 → "1.50s", 1234 → "1.23s"). 0 → "-"
// (matching the "not measured" convention used elsewhere).
func HumanizeMillis(ms float64) string {
	if ms <= 0 {
		return "-"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", int(ms+0.5))
	}
	return fmt.Sprintf("%.2fs", ms/1000)
}

// HumanizeSeconds renders a millisecond duration as seconds only —
// used for the "p95 latency" row in the diff where the user
// consistently uses seconds ("2.91s"). 0 → "-".
func HumanizeSeconds(ms float64) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2fs", ms/1000)
}

// HumanizeCount formats an integer with thousands separators (182 →
// "182", 8421 → "8,421") or "0" for zero.
func HumanizeCount(n int) string {
	if n == 0 {
		return "0"
	}
	return formatCommaInt(n)
}

// HumanizeTokens renders a token count with the user's compact
// suffix: < 1000 → plain ("421"), >= 1000 → "8.2k" with one decimal.
// We deliberately keep this terser than HumanizeCount because tokens
// are always large; thousands separators add noise.
//
// 999 → "999", 1000 → "1.0k", 8421 → "8.4k", 8420 → "8.4k"
// (rounding is round-half-to-even at the last decimal via fmt).
func HumanizeTokens(n int) string {
	if n <= 0 {
		return "0"
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000.0)
}

// HumanizeCost renders a dollar amount with adaptive precision:
// >= $1 → "$14.74", < $1 → "$0.081" (three decimals matches the
// user's sketch: "$0.081 → 0.057"). Zero → "$0.00".
func HumanizeCost(usd float64) string {
	if usd <= 0 {
		return "$0.00"
	}
	if usd >= 1.0 {
		return fmt.Sprintf("$%.2f", usd)
	}
	return fmt.Sprintf("$%.3f", usd)
}

// HumanizeCostBare is the suffix side of a diff row — same numeric
// formatting as HumanizeCost but without the leading "$". It pairs
// with HumanizeCost to render the user's "cost/request: $0.081 →
// 0.057" style. Zero stays "0.00" (no currency symbol, by design).
func HumanizeCostBare(usd float64) string {
	if usd <= 0 {
		return "0.00"
	}
	if usd >= 1.0 {
		return fmt.Sprintf("%.2f", usd)
	}
	return fmt.Sprintf("%.3f", usd)
}

// Humanize renders a token count or latency-style number for the
// generic "humanize this int" case used by RenderShow. We keep it
// separate from HumanizeTokens so callers don't accidentally cross
// the units. Currently unused outside the package; exported because
// future code (TUI, exporters) will want it.
func Humanize(n int) string {
	return HumanizeCount(n)
}

// formatCommaInt inserts thousands separators: 8421 → "8,421".
func formatCommaInt(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
		if len(s) > rem {
			b.WriteByte(',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		if i > rem {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}