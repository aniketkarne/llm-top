package ui

import (
	"fmt"
	"strings"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
)

// KeyMsg is the minimal key-event shape the UI consumes. The package
// stays bubbletea-free, so callers translate from bubbletea.KeyMsg or
// raw stdin runes themselves; we only need the rune + a small set of
// well-known specials (Esc, Enter, arrows).
type KeyMsg struct {
	Rune rune
	Esc  bool
	Entr bool
}

// UpdateResult carries the bookkeeping the existing Run() loop needs
// after applying a key: a possibly-updated model and a quit flag.
// Returning the model (rather than mutating in place) keeps Update
// testable as a pure function — same shape as bubbletea's signature.
type UpdateResult struct {
	Model *Model
	Quit  bool
}

// Update applies one KeyMsg to the model and returns the resulting
// state. The handling is intentionally minimal — the live Run() loop
// still drives refreshes on its own ticker — but every keybinding
// the Step 7 plan calls out is covered:
//
//   j / k  row nav (down / up)
//   Enter  toggle inline detail pane on the focused row
//   Esc    collapse the detail pane; clears overlays too
//   d      suggest a diff between the focused row and its neighbor
//   r      suggest a replay command for the focused row
//   a      open the anomalies overlay
//   s      open the session picker overlay
//   ?      open the help overlay
//   q      quit
//
// Keys only mutate when an overlay is closed; with an overlay up, j
// and k move an overlay-local cursor, Enter applies the selection,
// and Esc closes the overlay.
func Update(m *Model, msg KeyMsg) UpdateResult {
	if m == nil {
		return UpdateResult{Quit: true}
	}

	// Quit is honored regardless of overlay state.
	if msg.Rune == 'q' {
		return UpdateResult{Model: m, Quit: true}
	}

	// Esc first: it always closes whatever is on top.
	if msg.Esc {
		if m.overlay != OverlayNone {
			m.overlay = OverlayNone
			m.overlayCursor = 0
			return UpdateResult{Model: m}
		}
		if m.expandedRow != -1 {
			m.expandedRow = -1
			return UpdateResult{Model: m}
		}
		return UpdateResult{Model: m}
	}

	// With an overlay up: route j/k to the local cursor and Enter
	// to apply.
	if m.overlay != OverlayNone {
		switch {
		case msg.Rune == 'j' || msg.Rune == '↓':
			m.overlayCursor++
		case msg.Rune == 'k' || msg.Rune == '↑':
			if m.overlayCursor > 0 {
				m.overlayCursor--
			}
		case msg.Entr || msg.Rune == ' ':
			m = applyOverlaySelection(m)
			return UpdateResult{Model: m}
		case msg.Rune == 'a', msg.Rune == 's', msg.Rune == '?':
			// Switch overlays directly. The next j/k presses
			// hit the new overlay.
			m = openOverlay(m, msg.Rune)
		default:
			// Ignore unknown keys while an overlay is up.
		}
		m = m.ClampOverlayCursor()
		return UpdateResult{Model: m}
	}

	// No overlay: route to the activity list / inline expand pane.
	switch {
	case msg.Rune == 'j' || msg.Rune == '↓':
		rows := len(m.activityEntries())
		if rows == 0 {
			m.activityCursor = 0
		} else if m.activityCursor < rows-1 {
			m.activityCursor++
		}
	case msg.Rune == 'k' || msg.Rune == '↑':
		if m.activityCursor > 0 {
			m.activityCursor--
		}
	case msg.Entr || msg.Rune == ' ':
		// Toggle expand on the focused row.
		rows := len(m.activityEntries())
		if rows == 0 {
			m.expandedRow = -1
		} else if m.expandedRow == m.activityCursor {
			m.expandedRow = -1
		} else {
			m.expandedRow = m.activityCursor
		}
		m.SetHint("")
	case msg.Rune == 'd':
		m = hintForDiff(m)
	case msg.Rune == 'r':
		m = hintForReplay(m)
	case msg.Rune == 'a', msg.Rune == 's', msg.Rune == '?':
		m = openOverlay(m, msg.Rune)
	default:
		// Unknown key: ignore, keeps the cursor stable.
	}

	// Clamp the cursor after every change so wrapped rings can't
	// leave the cursor pointing off the end of the visible rows.
	m.ClampCursor()
	return UpdateResult{Model: m}
}

// openOverlay routes the given rune to its overlay kind. The
// overlay-specific list (e.g. anomalies from the store) is fetched
// lazily inside the renderer; here we only toggle state.
func openOverlay(m *Model, r rune) *Model {
	switch r {
	case 'a':
		m.overlay = OverlayAnomalies
	case 's':
		m.overlay = OverlaySessions
	case '?':
		m.overlay = OverlayHelp
	}
	m.overlayCursor = 0
	return m
}

// applyOverlaySelection handles Enter inside an overlay. The session
// picker is the only overlay that has an effect: it updates
// ActiveSessionID and closes the overlay. The anomalies overlay
// just closes (read-only); help closes too.
func applyOverlaySelection(m *Model) *Model {
	switch m.overlay {
	case OverlaySessions:
		sessions := sessionList(m)
		if m.overlayCursor < len(sessions) {
			m.ActiveSessionID = sessions[m.overlayCursor].ID
		}
		m.overlay = OverlayNone
		m.overlayCursor = 0
	case OverlayAnomalies, OverlayHelp:
		m.overlay = OverlayNone
		m.overlayCursor = 0
	}
	return m
}

// ClampOverlayCursor keeps overlayCursor inside the bounds of the
// currently-shown overlay's list. Returns the model for parity with
// ClampCursor.
func (m *Model) ClampOverlayCursor() *Model {
	max := overlayListLen(m)
	if max == 0 {
		m.overlayCursor = 0
	} else if m.overlayCursor < 0 {
		m.overlayCursor = 0
	} else if m.overlayCursor >= max {
		m.overlayCursor = max - 1
	}
	return m
}

// overlayListLen returns the number of rows the current overlay
// would show, used by the cursor clamp and by the renderer to decide
// if it should render "(empty)" placeholders.
func overlayListLen(m *Model) int {
	switch m.overlay {
	case OverlaySessions:
		return len(sessionList(m))
	case OverlayAnomalies:
		return len(anomalyList(m))
	case OverlayHelp:
		return 0 // help has no scrollable list
	}
	return 0
}

// sessionList is a tiny helper so the cursor math and the renderer
// agree on the same ordering.
func sessionList(m *Model) []SessionInfo {
	if m.Session == nil {
		return nil
	}
	return m.Session.List()
}

// anomalyList is the anomaly counterpart. The TUI reads anomalies
// from the AnomalySource when one is configured; tests inject a
// StaticAnomalySource that already holds a Count + ByID map, so we
// derive a synthesized slice here for the overlay renderer.
//
// We deliberately do NOT load from SQLite in real wiring here: the
// store-backed source is wired in main.go behind the sqlite tag, and
// when that source is configured this function falls back to the
// nil slice (the overlay renders "(no anomalies yet)").
func anomalyList(m *Model) []string {
	if m.Anomalies == nil {
		return nil
	}
	// We don't keep a full list on the source (it's a count +
	// per-id lookup interface). The renderer falls back to the
	// summary banner when the slice is empty, which matches the
	// "headline count" story for live captures: full per-row
	// detail lives behind `llm-top anomalies` CLI.
	if m.Anomalies.Count() == 0 {
		return nil
	}
	// Synthesize a single summary line so the overlay renders
	// something meaningful even without a per-id list.
	return []string{fmt.Sprintf("%d anomaly record(s) captured — `llm-top anomalies` for full detail", m.Anomalies.Count())}
}

// hintForDiff produces the status-line hint for the "diff two
// adjacent rows" flow. The TUI doesn't actually fork the CLI — it
// tells the user which command to run in another terminal. This is
// the safe default that avoids terminal focus stealing and matches
// the parent's design note about not forking inside the TUI.
func hintForDiff(m *Model) *Model {
	rows := m.activityEntries()
	if len(rows) < 2 {
		return m.SetHint("diff needs at least two rows")
	}
	// Cursor points at the "newer" row in user mental model; pair
	// it with the row directly above (older). If they happen to
	// be the same entry (e.g. cursor at 0) we bail with a hint.
	if m.activityCursor == 0 {
		return m.SetHint("diff needs at least two rows")
	}
	older := ringSource(rows[m.activityCursor-1])
	newer := ringSource(rows[m.activityCursor])
	hint := strings.Builder{}
	hint.WriteString("diff requested: ")
	hint.WriteString(older)
	hint.WriteString(" vs ")
	hint.WriteString(newer)
	hint.WriteString("\n  run: llm-top diff ")
	hint.WriteString(safeID(older))
	hint.WriteByte(' ')
	hint.WriteString(safeID(newer))
	return m.SetHint(hint.String())
}

// hintForReplay produces the status-line hint for replaying the
// focused row.
func hintForReplay(m *Model) *Model {
	rows := m.activityEntries()
	if len(rows) == 0 || m.activityCursor >= len(rows) {
		return m.SetHint("replay needs a focused row")
	}
	id := ringSource(rows[m.activityCursor])
	return m.SetHint(fmt.Sprintf("replay requested: run `llm-top replay %s` in another terminal", safeID(id)))
}

// ringSource extracts a short identifier from a ring entry. The
// existing ui.renderEntry uses `e.Source` (model name) plus a
// content preview; for the hint we surface the Source verbatim (the
// proxy.Request id is not yet plumbed into ring.Entry — that is a
// Step 8 ergonomics improvement).
func ringSource(e ring.Entry) string {
	if e.Source != "" {
		return e.Source
	}
	return fmt.Sprintf("%v", e.Time.Format("15:04:05"))
}

// safeID makes sure a hint value can't escape its enclosing string.
// Anything not matching [A-Za-z0-9_-] gets rendered as a "-" so the
// shell command we print is always tokenizable.
func safeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "row"
	}
	return out
}
