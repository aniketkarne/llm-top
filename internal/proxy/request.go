package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
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

// NewID returns a 16-hex-char ID derived from the current time and
// crypto-random bytes. The time prefix keeps IDs roughly sortable by
// creation order in human-readable listings; the random tail makes
// collisions effectively impossible without a heavyweight ULID dep.
//
// 16 hex chars = 64 bits: 32 bits of low-order UnixNano (repeats
// within a nanosecond, hence the 32 bits of random entropy).
func NewID() string {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	// Use the low 32 bits of UnixNano — UnixNano today is ~60 bits
	// wide so the upper bits would have widened the output beyond
	// 16 hex chars. 32 bits of nanoseconds (~4 seconds of range
	// before wraparound at year 2106) plus 32 bits of random gives
	// ~64 bits of entropy overall.
	ns := uint64(time.Now().UTC().UnixNano())
	return fmt.Sprintf("%08x%08x", uint32(ns), rnd)
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