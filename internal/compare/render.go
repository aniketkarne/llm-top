// Renderer for compare.CompareResult. Two output modes:
//
//   - text (default): a tabwriter-aligned side-by-side table that
//     matches the user's sketch: status row first, then TTFT, total,
//     input/output tokens, and cost. After the table, a short
//     "excerpts" section shows the first 200 chars of each
//     successful response body, one per line, prefixed with the
//     target name.
//
//   - json: a JSON object containing the full CompareResult. Useful
//     for piping compare output into jq or another tool.
//
// The text renderer is pure: it takes a []Row and a writer and writes
// nothing else to stdout. That keeps it deterministic for snapshot
// tests (see render_test.go) and easy to embed from the CLI without
// worrying about side effects.
package compare

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// RenderText writes the human-readable side-by-side table to w. The
// header row uses the target Name; value rows show status, TTFT,
// total, input/output tokens, and cost. Rows with Row.Error populated
// render "ERR" instead of a numeric status code; downstream metric
// cells render "-" so the column structure stays intact even when a
// target failed entirely.
//
// All numbers use the right-aligned tabwriter style from the spec:
// minwidth=12, tabwidth=1, padding=2. We use a single tabwriter
// instance for the whole table so header and value columns line up.
//
// capturedID is printed on the first line of output so the user knows
// which request the table belongs to.
func RenderText(w io.Writer, capturedID string, rows []Row) {
	// cellWidth is the column width every value cell is right-padded to.
	// Wider columns accommodate longer values (e.g. "10,421" prompt
	// tokens) without mis-aligning the rest. We compute it once and
	// reuse it for every row.
	const cellWidth = 12

	if len(rows) == 0 {
		fmt.Fprintln(w, "(no targets)")
		return
	}

	// Captured-id header. This anchors the output so a user with
	// several tabs of `compare` output knows which request the table
	// belongs to.
	fmt.Fprintf(w, "compare: captured=%s\n\n", capturedID)

	// Header row: target names. We use plain left-aligned tabwriter
	// (no AlignRight) because Go's tabwriter has an edge case with
	// right-alignment that collapses trailing tabs when all cells in
	// a row fit their column exactly. Numeric values are right-padded
	// manually by formatCell so they line up under the headers.
	tw := tabwriter.NewWriter(w, 12, 1, 2, ' ', 0)
	fmt.Fprint(tw, "          	")
	for _, r := range rows {
		fmt.Fprintf(tw, "%-*s", cellWidth, r.Name)
	}
	fmt.Fprintln(tw)

	// Divider under the header.
	fmt.Fprint(tw, "          	")
	for range rows {
		fmt.Fprint(tw, "────────────")
	}
	fmt.Fprintln(tw)

	// Per-row data. We render each row in its own tabwriter flush so
	// the column widths settle after the header.
	writeRow := func(label string, cell func(r Row) string) {
		fmt.Fprintf(tw, "%-10s	", label)
		for i, r := range rows {
			if i > 0 {
				fmt.Fprint(tw, "	")
			}
			// Right-pad each cell to cellWidth so the columns line up
			// under the headers regardless of content length.
			fmt.Fprintf(tw, "%-*s", cellWidth, cell(r))
		}
		fmt.Fprintln(tw)
	}

	writeRow("status", func(r Row) string {
		if r.Error != "" && r.StatusCode == 0 {
			return "ERR"
		}
		return fmt.Sprintf("%d", r.StatusCode)
	})
	writeRow("model", func(r Row) string {
		if r.Model == "" {
			return "-"
		}
		return r.Model
	})
	writeRow("provider", func(r Row) string {
		if r.Provider == "" {
			return "-"
		}
		return r.Provider
	})
	writeRow("ttft", func(r Row) string {
		if r.TTFTMillis <= 0 {
			return "-"
		}
		return fmt.Sprintf("%dms", r.TTFTMillis)
	})
	writeRow("total", func(r Row) string {
		if r.TotalMillis <= 0 {
			return "-"
		}
		// Render as seconds when >= 1000ms for readability — the
		// user's sketch shows "2.81s". Keep sub-second values in
		// ms for clarity (e.g. "420ms").
		if r.TotalMillis >= 1000 {
			return fmt.Sprintf("%.2fs", float64(r.TotalMillis)/1000.0)
		}
		return fmt.Sprintf("%dms", r.TotalMillis)
	})
	writeRow("input", func(r Row) string {
		if r.PromptTokens <= 0 {
			return "-"
		}
		return formatNumber(r.PromptTokens)
	})
	writeRow("output", func(r Row) string {
		if r.OutputTokens <= 0 {
			return "-"
		}
		return formatNumber(r.OutputTokens)
	})
	writeRow("cost", func(r Row) string {
		if !r.CostKnown {
			return "$?"
		}
		if r.CostUSD == 0 {
			return "$0.00"
		}
		// Two decimal places matches the user's sketch ($0.08, $0.05).
		return fmt.Sprintf("$%.2f", r.CostUSD)
	})

	if err := tw.Flush(); err != nil {
		// tabwriter on a writable Writer shouldn't fail; surface
		// it anyway so the caller can decide.
		fmt.Fprintln(w, "(render error:", err.Error(), ")")
		return
	}

	// Response excerpts: one line per successful target, prefixed
	// with the target name, body truncated to MaxExcerptBytes chars.
	// We only emit the section when at least one row produced a body.
	hasExcerpt := false
	for _, r := range rows {
		if r.ResponseBody != "" && r.Error == "" {
			hasExcerpt = true
			break
		}
	}
	if hasExcerpt {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "excerpts:")
		for _, r := range rows {
			if r.ResponseBody == "" {
				continue
			}
			ex := Excerpt(r.ResponseBody)
			fmt.Fprintf(w, "  %s: %s\n", r.Name, ex)
		}
	}

	// Errors section: print one line per failed target so the user
	// can see exactly what broke. This complements the table (where
	// ERR appears in the status cell) without dominating the output.
	hasError := false
	for _, r := range rows {
		if r.Error != "" {
			hasError = true
			break
		}
	}
	if hasError {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "errors:")
		for _, r := range rows {
			if r.Error == "" {
				continue
			}
			fmt.Fprintf(w, "  %s: %s\n", r.Name, r.Error)
		}
	}
}

// RenderJSON writes the CompareResult as pretty JSON to w. The
// CapturedID is in the top-level object; rows are an array. Matches
// the JSON shape tests use for machine-readable diffing.
func RenderJSON(w io.Writer, res CompareResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

// formatNumber renders an integer with thousands separators matching
// the user's sketch (8,421 / 734 / 812). Kept in this file because
// it's a presentation concern, not a comparison one.
func formatNumber(n int) string {
	if n < 0 {
		return "-" + formatNumber(-n)
	}
	// Manual loop to avoid pulling in a humanize dep. We work in
	// reverse: split the digits into groups of three from the right
	// and join with commas.
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
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
