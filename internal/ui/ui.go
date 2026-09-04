// Package ui renders the terminal dashboard that observes the proxy's
// ring buffer, metrics, and runtime counters.
//
// The implementation is a minimal hand-rolled TUI (no bubbletea dependency
// required to keep the binary small and self-contained) that draws three
// panes — metrics, recent activity, and status — refreshed on a ticker.
//
// Bubbletea/lipgloss-style aesthetics are approximated with ASCII rules
// and ANSI colors when stdout is a TTY. When stdout is redirected (e.g. in
// CI or pipes) the UI degrades to plain text and runs a non-blocking loop
// that emits a single dashboard snapshot before returning.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
)

// StatsProvider exposes the runtime counters the UI displays.
type StatsProvider interface {
	Stats() (total, errors uint64)
}

// Model holds the shared state the UI renders.
type Model struct {
	Recorder *metrics.Recorder
	Ring     *ring.Buffer
	Stats    StatsProvider
	Started  time.Time
	Upstream string
	Listen   string
	Width    int
	Height   int
	NoColor  bool
}

// New constructs a Model. Width/Height of 0 mean "use terminal size" /
// "no size limit".
func New(rec *metrics.Recorder, rb *ring.Buffer, sp StatsProvider, listen, upstream string) *Model {
	return &Model{
		Recorder: rec,
		Ring:     rb,
		Stats:    sp,
		Started:  time.Now(),
		Upstream: upstream,
		Listen:   listen,
	}
}

// RenderSnapshot produces a single dashboard frame as a string. Useful for
// tests and for piped output.
func (m *Model) RenderSnapshot() string {
	var b strings.Builder
	summary := m.Recorder.Summarize()
	total, errs := m.Stats.Stats()
	uptime := time.Since(m.Started).Truncate(time.Second)

	w := m.Width
	if w <= 0 {
		w = 100
	}

	b.WriteString(m.color("1;36", strings.Repeat("═", w)))
	b.WriteString("\n")
	title := fmt.Sprintf(" llm-top  •  listen=%s  upstream=%s  uptime=%s ", m.Listen, m.Upstream, uptime)
	b.WriteString(m.color("1;37", padRight(title, w)))
	b.WriteString("\n")
	b.WriteString(m.color("1;36", strings.Repeat("═", w)))
	b.WriteString("\n")

	// Metrics pane
	mets := []string{
		fmt.Sprintf(" requests      : %d", total),
		fmt.Sprintf(" errors        : %d", errs),
		fmt.Sprintf(" avg TTFT      : %s", summary.AvgTTFT),
		fmt.Sprintf(" avg total     : %s", summary.AvgTotal),
		fmt.Sprintf(" p50 total     : %s", summary.P50Total),
		fmt.Sprintf(" p95 total     : %s", summary.P95Total),
		fmt.Sprintf(" p99 total     : %s", summary.P99Total),
		fmt.Sprintf(" prompt tokens : %d", summary.TotalInTok),
		fmt.Sprintf(" output tokens : %d", summary.TotalOutTok),
	}
	b.WriteString(m.color("33", "── metrics ──"))
	b.WriteString("\n")
	for _, line := range mets {
		b.WriteString(line)
		b.WriteString("\n")
	}

	// Buffer pane
	b.WriteString(m.color("33", "── recent activity ──"))
	b.WriteString("\n")
	snap := m.Ring.Snapshot()
	if len(snap) == 0 {
		b.WriteString(" (no traffic yet)\n")
	} else {
		start := 0
		maxRows := 12
		if m.Height > 0 {
			maxRows = m.Height / 3
			if maxRows < 3 {
				maxRows = 3
			}
			if maxRows > len(snap) {
				maxRows = len(snap)
			}
		}
		if len(snap) > maxRows {
			start = len(snap) - maxRows
		}
		for _, e := range snap[start:] {
			b.WriteString(m.renderEntry(e, w))
			b.WriteString("\n")
		}
	}

	b.WriteString(m.color("1;36", strings.Repeat("═", w)))
	b.WriteString("\n")
	b.WriteString(m.color("90", " press Ctrl-C to quit "))
	b.WriteString("\n")
	return b.String()
}

func (m *Model) renderEntry(e ring.Entry, w int) string {
	kindColor := "37"
	switch e.Kind {
	case "request":
		kindColor = "36"
	case "response", "response:preview":
		kindColor = "32"
	case "response-stream":
		kindColor = "33"
	}
	ts := e.Time.Format("15:04:05")
	preview := e.Content
	preview = strings.ReplaceAll(preview, "\n", " ")
	if len(preview) > w-30 {
		preview = preview[:w-33] + "..."
	}
	header := fmt.Sprintf(" [%s] %-9s %-18s ", ts, e.Kind, truncate(e.Source, 18))
	return fmt.Sprintf("%s%s%s", m.color(kindColor, header), m.color("90", "| "), preview)
}

// Run renders the dashboard until ctx is done. It uses an internal ticker
// to refresh roughly every refresh interval. If stdout is not a TTY it
// emits one frame and returns.
func (m *Model) Run(out io.Writer, refresh time.Duration, isTTY bool) error {
	if !isTTY {
		_, err := io.WriteString(out, m.RenderSnapshot())
		return err
	}
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	// initial frame
	_, _ = io.WriteString(out, m.RenderSnapshot())
	for {
		// best-effort: detect terminal resize by querying COLUMNS env
		if cols := os.Getenv("COLUMNS"); cols != "" {
			var cw int
			fmt.Sscanf(cols, "%d", &cw)
			if cw > 0 {
				m.Width = cw
			}
		}
		_, _ = io.WriteString(out, m.RenderSnapshot())
		// move cursor to top
		_, _ = io.WriteString(out, "\033[H")
		<-ticker.C
	}
}

// color wraps an ANSI SGR around text, unless NoColor is set.
func (m *Model) color(code, s string) string {
	if m.NoColor {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
