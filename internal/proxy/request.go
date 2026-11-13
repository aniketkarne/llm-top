package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Request is the structured record of one proxied LLM call. Every proxied
// request produces one of these regardless of success or failure, and it is
// persisted to the SQLite RequestStore when sqlite is compiled in.
//
// The shape of this struct is load-bearing: subsequent features (replay,
// compare, sessions, anomaly detection, Prometheus) read from it. Resist
// casual additions; prefer new optional fields with a zero default.
type Request struct {
	// ID is a stable, short identifier (16 hex chars) that the proxy
	// emits back to the client as X-Request-Id so the caller can
	// correlate, and that store queries use to fetch a single record.
	ID string `json:"id"`

	// StartedAt and EndedAt are wall-clock UTC. EndedAt is the zero
	// value if the request never completed (e.g. client cancel).
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	// Method and Path are the inbound HTTP method and URL path (no
	// query string). Path is preserved exactly so the TUI can show it.
	Method string `json:"method"`
	Path   string `json:"path"`

	// Model is the model name parsed out of the JSON body or path.
	// Empty string if it could not be determined.
	Model string `json:"model"`

	// Upstream is the base URL of the upstream API (e.g.
	// "https://api.openai.com"). Provider is a normalized name
	// derived from the upstream host (openai, anthropic, local,
	// custom).
	Upstream  string `json:"upstream"`
	Provider  string `json:"provider"`

	// Stream is true when the response was sent as text/event-stream.
	Stream bool `json:"stream"`

	// StatusCode is the upstream's HTTP status; 0 if the proxy never
	// reached upstream (e.g. DNS error, connect refused).
	StatusCode int `json:"status_code"`

	// Error is a human-readable error message for failed requests,
	// empty on success. Set by the proxy on transport / parse errors.
	Error string `json:"error,omitempty"`

	// TTFTMillis is time-to-first-token in milliseconds. Zero for
	// non-streaming responses.
	TTFTMillis int64 `json:"ttft_ms"`

	// TotalMillis is total wall-clock latency in milliseconds.
	TotalMillis int64 `json:"total_ms"`

	// PromptTokens is the input token count (estimated if upstream
	// did not return a count). OutputTokens is the count of tokens in
	// the assistant's response, also estimated when unavailable.
	PromptTokens  int `json:"prompt_tokens"`
	OutputTokens  int `json:"output_tokens"`

	// CostUSD is the estimated cost in US dollars. Zero when the model
	// is not in the pricing table.
	CostUSD float64 `json:"cost_usd"`

	// RequestBody is the redacted request body as a string.
	// Callers MUST redact secrets (Authorization headers, API keys)
	// before assigning; the store does not second-guess what it
	// receives.
	RequestBody string `json:"request_body,omitempty"`

	// ResponseBody is the redacted response body. For streaming
	// responses this is the concatenated text of the delta events.
	ResponseBody string `json:"response_body,omitempty"`

	// PromptHash is a sha256 hex digest of the normalized prompt
	// portion of the request, used for repeated-prompt detection.
	// Empty when no prompt is identifiable.
	PromptHash string `json:"prompt_hash"`

	// SessionID ties this request to an explicit session when one
	// is active. Empty when no session is running.
	SessionID string `json:"session_id,omitempty"`
}

// NewID returns a 16-hex-char ID that is strictly monotonic per process:
// every call returns a string greater (in lexicographic order) than the
// previous one. This lets the SQLite sort tie-break on ID work
// deterministically even when two records share the same StartedAt
// nanosecond (e.g. fast unit tests, sessions created in the same
// goroutine tick).
//
// 16 hex chars = 64 bits: the upper 32 bits of UnixNano as the time
// prefix (monotonic across the full nanosecond range until year 2554)
// plus a 32-bit atomic counter as the tail. The counter is seeded
// from crypto/rand at process startup so two simultaneously-running
// llm-top instances are very unlikely to pick the same starting
// sequence and collide within the same nanosecond.
func NewID() string {
	ns := uint64(time.Now().UTC().UnixNano())
	prefix := uint32(ns >> 32)
	seq := newIDCounter.Add(1)
	return fmt.Sprintf("%08x%08x", prefix, uint32(seq))
}

// newIDCounter holds the per-process monotonic counter. Seeded once
// at package init: 31 random bits from crypto/rand so two llm-top
// processes started at the same nanosecond rarely share a starting
// sequence, with the top bit kept clear so the 32-bit tail has ~2^31
// increments of headroom before it could wrap inside one ~4.3-second
// time-prefix window (a wrap would break monotonicity; 2^31 NewID
// calls per window is unreachable at any real call rate).
var newIDCounter atomic.Uint64

func init() {
	var seed [4]byte
	_, _ = rand.Read(seed[:])
	// 31-bit seed: top bit of the first byte cleared.
	newIDCounter.Store(uint64(seed[0]&0x7f)<<24 | uint64(seed[1])<<16 |
		uint64(seed[2])<<8 | uint64(seed[3]))
}

// HashPrompt returns the lowercase sha256 hex digest of the user-role
// messages extracted from a JSON request body. Returns "" when no
// messages array is present. Whitespace is collapsed so that
// semantically-identical prompts hash to the same value.
func HashPrompt(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var v struct {
		Messages []msg `json:"messages"`
	}
	if err := json.Unmarshal(body, &v); err != nil || len(v.Messages) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range v.Messages {
		if m.Role != "user" {
			continue
		}
		b.WriteString(strings.Join(strings.Fields(m.Content), " "))
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ProviderFromUpstream normalizes an upstream base URL into a short
// provider name used as a label throughout the proxy. Custom hosts
// fall back to "custom".
func ProviderFromUpstream(base string) string {
	if base == "" {
		return ""
	}
	// Strip scheme.
	s := base
	switch {
	case strings.HasPrefix(s, "https://"):
		s = strings.TrimPrefix(s, "https://")
	case strings.HasPrefix(s, "http://"):
		s = strings.TrimPrefix(s, "http://")
	}
	// Take just the host portion (no path).
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	host := strings.ToLower(s)
	switch {
	case strings.Contains(host, "api.openai.com"):
		return "openai"
	case strings.Contains(host, "anthropic.com"):
		return "anthropic"
	case strings.Contains(host, "localhost") || strings.Contains(host, "127.0.0.1") || strings.Contains(host, "0.0.0.0"):
		return "local"
	case strings.Contains(host, "11434"):
		return "local"
	}
	return "custom"
}