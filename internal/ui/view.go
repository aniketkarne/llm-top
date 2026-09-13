package ui

import (
	"fmt"
	"strings"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/anomaly"
)

// RenderView produces the full Step 7 dashboard: header with banner,
// the existing metrics pane, the activity list with cursor + per-row
// anomaly markers, an inline detail pane when a row is expanded, the
// overlay (if any), and a status footer with keybinding hints.
//
// This is the "rich" view the new Run() loop calls on each tick.
// RenderSnapshot() is preserved for the legacy single-frame path used
// in piped output and tests.
//
// The function is pure: it reads the model state and writes to the
// strings.Builder. No terminal side effects.
func (m *Model) RenderView() string {
	var b strings.Builder
	w := m.Width
	if w <= 0 {
		w = 100
	}
	h := m.Height
	if h <= 0 {
		h = 30
	}

	// ---- header (banner-aware) ----
	b.WriteString(m.color("1;36", strings.Repeat("═", w)))
	b.WriteByte('\n')

	// Banner line: red on yellow background, only when count > 0.
	if cnt := m.AnomalyCount(); cnt > 0 {
		// We render the banner as its own line so the existing
		// title row stays compact. ANSI 41 = red bg, 1;37 = bold white.
		banner := bannerText(cnt)
		b.WriteString(m.color("1;37;41", padRight(banner, w)))
		b.WriteByte('\n')
	}

	title := fmt.Sprintf(" llm-top  •  listen=%s  upstream=%s  •  j/k nav  Enter expand  a/s/?  Ctrl-C quit ", m.Listen, m.Upstream)
	b.WriteString(m.color("1;37", padRight(title, w)))
	b.WriteByte('\n')
	b.WriteString(m.color("1;36", strings.Repeat("═", w)))
	b.WriteByte('\n')

	// If an overlay is open, render it full-screen instead of the
	// activity / metrics panes. We still keep the header so context
	// (banner, listen/upstream) is visible.
	if m.overlay != OverlayNone {
		b.WriteString(m.renderOverlay(w))
		b.WriteString(m.color("1;36", strings.Repeat("═", w)))
		b.WriteByte('\n')
		b.WriteString(m.statusFooter(w))
		b.WriteByte('\n')
		return b.String()
	}

	// ---- metrics (kept compact — the focus of Step 7 is below) ----
	if m.Recorder != nil {
		summary := m.Recorder.Summarize()
		total, errs := m.Stats.Stats()
		b.WriteString(m.color("33", "── metrics ──"))
		b.WriteByte('\n')
		mets := []string{
			fmt.Sprintf(" requests      : %d", total),
			fmt.Sprintf(" errors        : %d", errs),
			fmt.Sprintf(" avg TTFT      : %s", summary.AvgTTFT),
			fmt.Sprintf(" avg total     : %s", summary.AvgTotal),
			fmt.Sprintf(" p95 total     : %s", summary.P95Total),
			fmt.Sprintf(" output tokens : %d", summary.TotalOutTok),
		}
		for _, line := range mets {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	// ---- activity list with cursor + per-row anomaly badges ----
	b.WriteString(m.color("33", "── recent activity ──"))
	b.WriteByte('\n')
	rows := m.activityEntries()
	if len(rows) == 0 {
		b.WriteString(" (no traffic yet)\n")
	} else {
		for i, e := range rows {
			b.WriteString(m.renderRow(i, e, w))
			b.WriteByte('\n')
		}
	}

	// ---- inline expand pane ----
	if m.expandedRow >= 0 && m.expandedRow < len(rows) {
		b.WriteString(m.renderExpand(rows[m.expandedRow], w))
		b.WriteByte('\n')
	}

	// ---- status footer (hint + keymap) ----
	b.WriteString(m.color("1;36", strings.Repeat("═", w)))
	b.WriteByte('\n')
	b.WriteString(m.statusFooter(w))
	b.WriteByte('\n')
	return b.String()
}

// AnomalyCount is the convenience wrapper used by RenderView. When no
// source is configured, returns 0 (banner hidden).
func (m *Model) AnomalyCount() int {
	if m.Anomalies == nil {
		return 0
	}
	return m.Anomalies.Count()
}

// renderRow is the per-row activity entry renderer. It extends
// renderEntry from ui.go with a cursor highlight + a per-row anomaly
// marker pulled from the AnomalySource.
func (m *Model) renderRow(idx int, e ring.Entry, w int) string {
	// Default layout: timestamp, kind, source, badges, preview.
	ts := e.Time.Format("15:04:05")
	kindColor := "37"
	switch e.Kind {
	case "request":
		kindColor = "36"
	case "response", "response:preview":
		kindColor = "32"
	case "response-stream":
		kindColor = "33"
	}

	preview := strings.ReplaceAll(e.Content, "\n", " ")
	maxPreview := w - 50
	if maxPreview < 8 {
		maxPreview = 8
	}
	if len(preview) > maxPreview {
		preview = preview[:maxPreview-3] + "..."
	}

	badges := m.badgesForRow(e)

	// Cursor: "▶ " prefix + reverse-video on the row.
	cursor := "  "
	rowColor := kindColor
	if idx == m.activityCursor {
		cursor = "▶ "
		rowColor = "1;" + kindColor // bold
	}
	expanded := ""
	if idx == m.expandedRow {
		expanded = "▼"
	} else {
		expanded = " "
	}

	header := fmt.Sprintf(" %s%s [%s] %-9s %-18s ", cursor, expanded, ts, e.Kind, truncate(e.Source, 18))
	line := fmt.Sprintf("%s %s %s", m.color(rowColor, header), m.color("90", "|"), preview)
	if badges != "" {
		line += "  " + m.color("33", badges)
	}
	return line
}

// badgesForRow returns the formatted anomaly badge(s) for a row, or
// an empty string when no anomalies are recorded. Joins multiple
// kinds with a space so e.g. one row that triggered both a TTFT
// spike and an error burst shows both.
func (m *Model) badgesForRow(e ring.Entry) string {
	if m.Anomalies == nil {
		return ""
	}
	id := rowID(e)
	if id == "" {
		return ""
	}
	kinds := m.Anomalies.KindsForRequest(id)
	if len(kinds) == 0 {
		return ""
	}
	seen := map[anomaly.Kind]bool{}
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if seen[k] {
			continue
		}
		seen[k] = true
		parts = append(parts, anomalyBadge(k))
	}
	return strings.Join(parts, " ")
}

// rowID is a stable, deterministic id for an entry — used to look up
// per-row anomaly kinds. The ring buffer doesn't carry the proxy
// Request ID today (a Step 8 ergonomics improvement), so we synthesize
// one from (timestamp, source) which is unique enough for live-cursor
// purposes: within the visible window the cursor never collides, and
// any duplicate hashes only ever deduplicate badges on the same row.
func rowID(e ring.Entry) string {
	return fmt.Sprintf("%d-%s", e.Time.UnixNano(), e.Source)
}

// renderExpand produces the inline detail pane shown beneath the
// activity list when a row is expanded. It shows the captured
// prompt (request body) and response (response body) for the
// selected row, separated by a divider. Falls back to a generic
// "(no body captured)" when the entry has neither.
func (m *Model) renderExpand(e ring.Entry, w int) string {
	var b strings.Builder
	b.WriteString(m.color("33", "── detail ──"))
	b.WriteByte('\n')
	body := e.Content
	if body == "" {
		b.WriteString(" (no body captured)\n")
		return b.String()
	}
	// Split on the first newline that separates prompt and response
	// entries are stored as single-line previews in the ring, so we
	// just show the content with a label.
	label := "prompt"
	if e.Kind == "response" || e.Kind == "response-stream" || e.Kind == "response:preview" {
		label = "response"
	}
	b.WriteString(fmt.Sprintf(" %s: ", label))
	maxBody := w - 4
	if maxBody < 8 {
		maxBody = 8
	}
	// Collapse newlines so the pane stays single-line and matches
	// the dashboard width.
	preview := strings.ReplaceAll(body, "\n", " ")
	if len(preview) > maxBody {
		preview = preview[:maxBody-3] + "..."
	}
	b.WriteString(preview)
	b.WriteByte('\n')
	return b.String()
}

// renderOverlay renders the active overlay as a full-width block
// between the header and footer. The block includes a title line,
// the (possibly-empty) list, and a contextual hint.
func (m *Model) renderOverlay(w int) string {
	var b strings.Builder
	switch m.overlay {
	case OverlayAnomalies:
		b.WriteString(m.color("33", "── anomalies (a/Esc to close) ──"))
		b.WriteByte('\n')
		list := anomalyList(m)
		if len(list) == 0 {
			b.WriteString(" (no anomalies)\n")
		} else {
			for _, s := range list {
				b.WriteString(" • ")
				b.WriteString(s)
				b.WriteByte('\n')
			}
		}
	case OverlaySessions:
		b.WriteString(m.color("33", "── session picker (s/Esc to close) ──"))
		b.WriteByte('\n')
		sessions := sessionList(m)
		if len(sessions) == 0 {
			b.WriteString(" (no sessions — start one with `llm-top session start`)\n")
		} else {
			for i, s := range sessions {
				marker := "  "
				if i == m.overlayCursor {
					marker = "▶ "
				}
				line := fmt.Sprintf("%s%s  %s\n", marker, s.ID, sessionLabel(s))
				b.WriteString(line)
			}
		}
	case OverlayHelp:
		b.WriteString(m.color("33", "── help (?) ──"))
		b.WriteByte('\n')
		for _, line := range []string{
			" j / k        move cursor down / up",
			" Enter / Spc  toggle inline detail pane on focused row",
			" Esc          collapse detail / close overlay",
			" d            suggest `llm-top diff A B` for the two focused rows",
			" r            suggest `llm-top replay <id>` for the focused row",
			" a            anomalies overlay (summary + count)",
			" s            session picker overlay",
			" ?            this help overlay",
			" q / Ctrl-C   quit",
		} {
			b.WriteString(" ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// sessionLabel composes a one-line summary for a session row.
func sessionLabel(s SessionInfo) string {
	name := s.Name
	if name == "" {
		name = s.Label
	}
	if name == "" {
		name = "(unnamed)"
	}
	if s.StartedAt.IsZero() {
		return name
	}
	if s.EndedAt.IsZero() {
		return fmt.Sprintf("%s — started %s (active)", name, s.StartedAt.Format("15:04:05"))
	}
	return fmt.Sprintf("%s — %s → %s", name, s.StartedAt.Format("15:04:05"), s.EndedAt.Format("15:04:05"))
}

// statusFooter renders the bottom-of-screen status line: a brief
// keymap reminder (always shown) plus the optional d/r hint when one
// is set. The hint is shown verbatim; it is the caller's job to keep
// it short.
func (m *Model) statusFooter(w int) string {
	left := m.color("90", " j/k • Enter • d=r  a/s/? • Esc • q ")
	right := m.lastHint
	if right == "" {
		return left
	}
	maxRight := w - len(left) - 2
	if maxRight < 8 {
		maxRight = 8
	}
	if len(right) > maxRight {
		right = right[:maxRight-3] + "..."
	}
	return left + m.color("33", "  "+right)
}
