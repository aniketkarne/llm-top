// Package session provides first-class Session records for llm-top.
//
// A Session is a logical bucket of proxied requests — start one when
// you begin a benchmarking run, end it when you're done, then use
// session show / session diff to compare against a baseline.
//
// The package is split into three concerns:
//
//   - session.go: the Manager type, which owns the Session record and
//     talks to internal/store for SQLite persistence.
//   - aggregate.go: pure aggregation over []proxy.Request —
//     AggregateStats and DeltaStats. Zero side effects.
//   - render.go: pure text rendering — RenderShow, RenderDiff, and a
//     small Humanize helper. Zero side effects.
//
// The aggregation and rendering are deliberately separate from the
// Manager so they can be unit-tested with synthetic data and reused
// by the TUI (Step 7) without pulling in SQLite.
package session