package ui

import (
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
)

// StatsProvider exposes the runtime counters the UI displays.
type StatsProvider interface {
	Stats() (total, errors uint64)
}

// OverlayKind enumerates the full-screen modals the TUI can render
// on top of the activity list. Only one overlay is visible at a time;
// switching overlays simply replaces the previous one.
type OverlayKind int

const (
	OverlayNone OverlayKind = iota
	OverlayAnomalies
	OverlaySessions
	OverlayHelp
)

// Model holds the shared state the UI renders. The Step 7 upgrade
// adds anomaly sourcing, cursor nav, an inline expand pane, and a
// pluggable full-screen overlay (anomalies list / session picker /
// help). The legacy fields (Recorder, Ring, Stats, etc.) are kept
// untouched so the existing tests, the existing Run() loop, and the
// simple RenderSnapshot() path all keep working unchanged.
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

	// --- Step 7 additions ---

	// Anomalies is the source-of-truth for live anomaly counts and
	// per-request anomaly kinds. nil means "no anomalies" and the
	// banner stays hidden. Wired in main.go when cfg.Mode is "ui"
	// or "integrated" and the SQLite store (or proxy /metrics
	// endpoint) is available.
	Anomalies AnomalySource

	// Session is the active session id (empty when no session is
	// running). Future requests are tagged with this id; the
	// session picker overlay calls SetActiveSession to switch.
	// Holds a back-reference to the session source when one is
	// available so the picker can list candidates. nil-safe.
	Session SessionSource
	// ActiveSessionID is the currently selected session filter.
	// Empty string means "all requests".
	ActiveSessionID string

	// activityCursor is the row index currently focused in the
	// activity list. 0 is the most recent row once the list is
	// paginated; the activity list is rendered newest-last, so
	// "up" actually moves toward newer rows in user mental model.
	// We clamp on every keypress.
	activityCursor int

	// expandedRow is the row index the inline detail pane is open
	// for, or -1 when collapsed. Toggled by Enter / Esc.
	expandedRow int

	// overlay is the currently-shown full-screen modal. OverlayNone
	// means the activity list is the primary view.
	overlay OverlayKind

	// overlayCursor is the selected row inside a list overlay
	// (anomalies / sessions). j/k move it; Enter applies.
	overlayCursor int

	// lastHint is the most recent hint or message we surfaced via
	// the status line (e.g. "run `llm-top diff A B` to compare
	// rows 4 and 5"). It is shown briefly then cleared so the
	// caller doesn't have to manage a confirmation UI on top of
	// the existing dashboard.
	lastHint string
}

// SessionSource is the minimal interface the TUI uses to drive the
// session picker overlay. The SQLite store implements it for real
// (see main.go wiring); tests pass a fake.
//
// All methods are best-effort. Empty results and errors are both
// rendered as "(no sessions)" so callers can blindly print.
type SessionSource interface {
	// List returns recent sessions, newest first. Empty when no
	// sessions are persisted yet.
	List() []SessionInfo
}

// SessionInfo is the TUI-facing projection of a session. The store
// has its own Session struct; projecting here keeps the ui package
// from importing the SQLite build tag.
type SessionInfo struct {
	ID        string
	Name      string
	Label     string
	StartedAt time.Time
	EndedAt   time.Time
}

// WithAnomalySource attaches an anomaly source to the model. Returns
// the receiver so callers can chain in the same style as
// proxy.Server.WithXxx. Pass nil to keep the banner hidden.
func (m *Model) WithAnomalySource(src AnomalySource) *Model {
	m.Anomalies = src
	return m
}

// WithSessionSource attaches a session source and (optionally) sets
// the active session id. Both are best-effort; a nil source or empty
// id leaves the field zeroed so the picker overlay shows the empty
// state.
func (m *Model) WithSessionSource(src SessionSource, activeID string) *Model {
	m.Session = src
	m.ActiveSessionID = activeID
	return m
}

// activityEntries returns the rows currently shown in the activity
// pane — most-recent N. This is the same slice ordering the existing
// RenderSnapshot uses, so callers see consistent cursor math.
func (m *Model) activityEntries() []ring.Entry {
	if m.Ring == nil {
		return nil
	}
	snap := m.Ring.Snapshot()
	if m.Height > 0 {
		maxRows := m.Height / 3
		if maxRows < 3 {
			maxRows = 3
		}
		if maxRows > len(snap) {
			maxRows = len(snap)
		}
		if len(snap) > maxRows {
			snap = snap[len(snap)-maxRows:]
		}
	} else if len(snap) > 12 {
		// Match the legacy default in ui.go: cap at 12 rows
		// when the terminal height is unknown.
		snap = snap[len(snap)-12:]
	}
	return snap
}

// ClampCursor makes sure the cursor stays in range after the activity
// list changes length (an entry was appended, the ring wrapped, the
// window resized, etc.). Returns the model so it can be assigned back
// in tests and in update.go without surprising the caller.
func (m *Model) ClampCursor() *Model {
	rows := len(m.activityEntries())
	if rows == 0 {
		m.activityCursor = 0
	} else if m.activityCursor < 0 {
		m.activityCursor = 0
	} else if m.activityCursor >= rows {
		m.activityCursor = rows - 1
	}
	if m.expandedRow >= rows {
		m.expandedRow = -1
	}
	return m
}

// SetHint records a short status-line message to surface to the
// user. Used by d/r when the TUI wants to point at an external CLI
// command rather than fork/exec one inside the bubbletea-less loop.
func (m *Model) SetHint(msg string) *Model {
	m.lastHint = msg
	return m
}
