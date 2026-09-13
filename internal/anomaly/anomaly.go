// Package anomaly detects performance and quality anomalies in the live
// stream of captured LLM requests. The package is split into three files:
//
//   - anomaly.go (this file): the Anomaly struct and Kind/Severity
//     constants. Pure data; no logic.
//   - detector.go: the rolling-baseline Detector. Owns per-model
//     statistics and returns []Anomaly from each Evaluate call.
//   - render.go: pure text and JSON renderers for []Anomaly.
//
// The Detector is safe for concurrent use. The CLI and proxy share a
// SQLite-backed persistent store via store.InsertAnomaly /
// store.ListAnomalies; the in-memory Detector is the proxy's hot path
// and the store is the cold / queryable path.
package anomaly

import (
	"encoding/json"
	"time"
)

// Kind classifies what an anomaly is about. Stored verbatim in the
// `anomalies.kind` column and exposed in JSON output for filtering.
type Kind string

// Severity is "info", "warn", or "alert". It is stored verbatim and
// used by renderers to color / prefix rows.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityAlert Severity = "alert"
)

const (
	// KindTTFTSpike fires when a streaming request's TTFT is more than
	// TTFTMultiplier x the rolling p95 of TTFT for the same model.
	KindTTFTSpike Kind = "ttft_spike"

	// KindLatencySpike fires when total latency is more than
	// LatencyMultiplier x the rolling p95 of total latency for the same
	// model.
	KindLatencySpike Kind = "latency_spike"

	// KindTokenExplosion fires when output tokens are more than
	// TokenMultiplier x the rolling mean for the same model.
	KindTokenExplosion Kind = "token_explosion"

	// KindRepeatedPrompt fires when the same prompt_hash has fired at
	// least RepeatedPromptCount times within RepeatedPromptWindow.
	KindRepeatedPrompt Kind = "repeated_prompt"

	// KindErrorBurst fires when the fraction of errored requests in
	// the last ErrorBurstWindow is above ErrorBurstThreshold.
	KindErrorBurst Kind = "error_burst"

	// KindLargeContext fires when input tokens exceed LargeContextTokens.
	KindLargeContext Kind = "large_context"

	// KindProviderDegrade fires when one provider's error rate for a
	// given model is much higher than the rolling baseline of other
	// providers for the same model.
	KindProviderDegrade Kind = "provider_degrade"
)

// Anomaly is one detected anomaly. The fields are chosen so that the
// text and JSON renderers both work without needing to know which
// Kind they are handling: Message is human-readable, Baseline and
// Observed are formatted numbers, Extra holds kind-specific context
// (e.g. provider name, prompt hash) for tooling.
//
// ID is auto-assigned by store.InsertAnomaly when empty (16-hex
// char, same shape as Request IDs). For in-memory Detector use, ID
// stays empty.
type Anomaly struct {
	ID         string            `json:"id,omitempty"`
	Kind       Kind              `json:"kind"`
	Severity   Severity          `json:"severity"`
	Message    string            `json:"message"`
	DetectedAt time.Time         `json:"detected_at"`
	RequestID  string            `json:"request_id,omitempty"`
	Baseline   string            `json:"baseline,omitempty"`
	Observed   string            `json:"observed,omitempty"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// EncodeExtra marshals Extra to a JSON string for storage. Empty map
// marshals to "{}" so downstream readers always see valid JSON.
func (a Anomaly) EncodeExtra() string {
	if a.Extra == nil {
		return "{}"
	}
	b, err := json.Marshal(a.Extra)
	if err != nil {
		return "{}"
	}
	return string(b)
}