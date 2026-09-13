// Package replay re-fires a captured Request against an upstream HTTP
// endpoint and returns a fresh Result describing the new round-trip. The
// package is intentionally pure: it does not read from or write to any
// persistence layer. Callers (the CLI subcommands in cmd/llm-top) decide
// what to do with the returned Result — typically print it, and
// optionally persist it for later comparison.
//
// The replayer is deliberately narrow:
//
//   - It accepts a captured body, optionally rewrites the "model" field
//     via surgical bytes.Replace (no re-marshal — preserves formatting
//     and indentation), and POSTs it to upstreamBaseURL + path.
//   - It accepts an Authorization header (the upstream API key) and an
//     Accept header that advertises both application/json and
//     text/event-stream, so the upstream can pick stream vs non-stream
//     based on the body's "stream" flag.
//   - It parses SSE when present (computes TTFT as time-to-first-event)
//     and falls back to reading the full body otherwise. Token counts
//     are estimated from the response text when usage metadata is
//     absent.
//
// No persistence is performed inside this package — that decision is
// pushed to the caller so the replayer stays testable and side-effect
// free.
package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/pricing"
	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// ExecutionConfig configures a single replay invocation. Zero values
// are sensible: a zero Timeout falls back to 30s; a zero Stream means
// "use the captured stream flag" (re-read from the body if absent).
type ExecutionConfig struct {
	// UpstreamBaseURL is the base URL the request will be POSTed to,
	// e.g. "https://api.openai.com" or "http://localhost:11434/v1".
	// Required.
	UpstreamBaseURL string

	// UpstreamAPIKey is sent verbatim as the Bearer token. Empty for
	// local Ollama-style upstreams that don't require auth.
	UpstreamAPIKey string

	// Model, when non-empty, overrides the "model" field in the JSON
	// request body via a surgical bytes.Replace. When empty the
	// captured body's model is used as-is.
	Model string

	// Stream, when non-nil, overrides the "stream" field in the JSON
	// body. When nil the captured body's stream flag is used.
	Stream *bool

	// Timeout bounds the entire request, including streaming. Default
	// 30 seconds when zero.
	Timeout time.Duration

	// Headers are extra request headers, in addition to Authorization
	// and Accept. Values are sent verbatim. Useful for tracing IDs.
	Headers map[string]string

	// Body, when non-empty, REPLACES the captured body entirely.
	// Used by `llm-top edit` after the user has finished modifying
	// the JSON in their editor. When empty the captured body is sent
	// as-is (after any model/stream overrides).
	Body string

	// Pricing, when non-zero, is used to compute the result's
	// CostUSD. When zero, pricing.Defaults() is used.
	Pricing pricing.Table
}

// Result describes one fresh round-trip produced by Execute. It is
// modeled after proxy.Request but is intentionally simpler — the
// replayer doesn't track everything the proxy does (e.g. prompt hash
// derivation, redaction).
type Result struct {
	// RequestID is the new ID for this replay attempt. Distinct from
	// the captured request's ID.
	RequestID string

	StartedAt time.Time
	EndedAt   time.Time

	StatusCode    int
	TTFTMillis    int64
	TotalMillis   int64
	PromptTokens  int
	OutputTokens  int
	CostUSD       float64
	Stream        bool
	ResponseBody  string
	Error         string

	// Target records the effective upstream/model/provider that
	// produced this result, so the caller can print "replayed against
	// <base> (<provider>) using <model>" without re-deriving it.
	Target Target
}

// Target identifies the destination that produced this Result.
type Target struct {
	BaseURL  string
	Model    string
	Provider string
}

// EffectiveModel returns the model that will be sent upstream, given a
// captured body and the execution config. It's exported so callers can
// surface "replaying as model=X" before firing the request.
func EffectiveModel(captured string, cfg ExecutionConfig) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	return captured
}

// Execute fires the captured request against the configured upstream
// and returns the result. It never panics on transport errors; instead
// it returns a Result with Error populated and StatusCode=0 so callers
// can render a partial report instead of crashing.
//
// The captured Request's Path is preserved exactly so chat-completion
// requests hit /v1/chat/completions on the upstream even when the
// upstream base URL has its own /v1 suffix.
func Execute(ctx context.Context, captured proxy.Request, cfg ExecutionConfig) Result {
	res := Result{
		RequestID: proxy.NewID(),
		StartedAt: time.Now().UTC(),
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if cfg.UpstreamBaseURL == "" {
		res.Error = "upstream base URL is empty (use --upstream, --local, or LLMTOP_UPSTREAM)"
		res.EndedAt = time.Now().UTC()
		res.TotalMillis = millisBetween(res.StartedAt, res.EndedAt)
		return res
	}

	// Build the body — prefer the override Body (from edit), then
	// apply model/stream rewrites to the captured body.
	bodyStr := cfg.Body
	if bodyStr == "" {
		bodyStr = captured.RequestBody
	}
	if bodyStr != "" {
		bodyStr = rewriteJSONField(bodyStr, "model", cfg.Model)
		bodyStr = rewriteJSONStream(bodyStr, cfg.Stream)
	}

	effectiveModel := EffectiveModel(captured.Model, cfg)
	res.Target = Target{
		BaseURL:  cfg.UpstreamBaseURL,
		Model:    effectiveModel,
		Provider: proxy.ProviderFromUpstream(cfg.UpstreamBaseURL),
	}

	// Compose the path the way the proxy would: base + captured.Path.
	// We don't try to be clever about "/v1" suffixes because the
	// proxy never was: it always concatenated base + path verbatim.
	url := joinURL(cfg.UpstreamBaseURL, captured.Path)

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, strings.NewReader(bodyStr))
	if err != nil {
		res.Error = "build request: " + err.Error()
		res.EndedAt = time.Now().UTC()
		res.TotalMillis = millisBetween(res.StartedAt, res.EndedAt)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if cfg.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.UpstreamAPIKey)
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	httpClient := &http.Client{
		Timeout: timeout,
		// Don't follow redirects — the proxy doesn't, and an LLM
		// upstream redirecting would be a configuration surprise.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	start := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		res.Error = "transport: " + err.Error()
		res.EndedAt = time.Now().UTC()
		res.TotalMillis = millisBetween(res.StartedAt, res.EndedAt)
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	res.StatusCode = resp.StatusCode
	ct := resp.Header.Get("Content-Type")

	// Decide stream vs non-stream from the upstream's response content
	// type (not the captured request's stream flag — the upstream may
	// have honored it or ignored it).
	isSSE := strings.HasPrefix(ct, "text/event-stream")
	res.Stream = isSSE

	if isSSE {
		text, ttft, err := readSSE(resp.Body, start)
		res.ResponseBody = text
		// ttft is a sub-millisecond precision duration; round UP to
		// the nearest ms so the caller sees something measurable.
		ttftMs := ttft.Milliseconds()
		if ttftMs == 0 && ttft > 0 {
			ttftMs = 1
		}
		res.TTFTMillis = ttftMs
		if err != nil {
			res.Error = "read sse: " + err.Error()
		}
	} else {
		b, err := io.ReadAll(resp.Body)
		res.ResponseBody = string(b)
		if err != nil {
			res.Error = "read body: " + err.Error()
		}
	}

	res.EndedAt = time.Now().UTC()
	res.TotalMillis = millisBetween(res.StartedAt, res.EndedAt)
	res.PromptTokens = estimatePromptTokens(captured.RequestBody)
	res.OutputTokens = estimateOutputTokens(res.ResponseBody)
	if cost, ok := costFor(cfg, effectiveModel, res.PromptTokens, res.OutputTokens); ok {
		res.CostUSD = cost
	}
	return res
}

// joinURL concatenates a base URL with a captured request path in a way
// that survives "host:port" (no scheme), "https://host/v1" (API prefix),
// and "https://host/v1/chat/completions" (full endpoint) shapes.
//
// The algorithm:
//  1. Normalize: prepend http:// if base has no scheme.
//  2. Strip trailing slashes from base and leading slashes from path.
//  3. If path is empty, return base.
//  4. If base ends with the full path, return base.
//  5. Otherwise find the longest trailing path-segment of base that is
//     also a leading segment of path, and strip that overlap from path
//     before joining. Example: base="/v1", path="/v1/chat/completions"
//     → "/v1/chat/completions".
//  6. Fallback: simple slash-join.
func joinURL(base, p string) string {
	if base == "" {
		return p
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	cleanBase := strings.TrimRight(base, "/")
	cleanPath := strings.TrimLeft(p, "/")
	if cleanPath == "" {
		return cleanBase
	}
	// Case 1: base already ends with the full captured path.
	if cleanBase == cleanPath || strings.HasSuffix(cleanBase, "/"+cleanPath) {
		return cleanBase
	}
	// Case 2: find the longest trailing segment of base that matches
	// a leading segment of path. We walk from longest possible match
	// down to length 1 to find the right overlap.
	basePath := pathAfterScheme(cleanBase)
	if basePath != "" {
		// Walk segments from longest to shortest.
		for n := len(basePath); n > 1; n-- {
			if n > len(basePath) {
				continue
			}
			trial := basePath[len(basePath)-n:]
			if !strings.HasPrefix(cleanPath, trial) {
				continue
			}
			// trial must end at a path-segment boundary in BOTH
			// basePath and cleanPath for this to be a real match.
			if !segmentBoundary(basePath, len(basePath)-n) {
				continue
			}
			if !segmentBoundary(cleanPath, 0) || len(cleanPath) < n {
				continue
			}
			// Boundary in cleanPath: either cleanPath == trial exactly,
			// or the next char in cleanPath is '/'.
			if len(cleanPath) == n || cleanPath[n] == '/' {
				rest := cleanPath[n:]
				rest = strings.TrimLeft(rest, "/")
				if rest == "" {
					return cleanBase
				}
				return cleanBase + "/" + rest
			}
		}
	}
	// Case 3: simple join.
	if strings.HasSuffix(base, "/") {
		return base + cleanPath
	}
	if strings.HasPrefix(p, "/") {
		return base + p
	}
	return base + "/" + p
}

// pathAfterScheme returns the path portion of a URL ("https://host/v1/x" → "/v1/x").
// Returns "" if there's no path (just a host).
func pathAfterScheme(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return ""
	}
	rest := u[i+3:]
	// Skip past host:port.
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return ""
	}
	return rest[slash:]
}

// segmentBoundary reports whether pos is at a path-segment boundary in s.
// A boundary is the start of s, or any position immediately after a '/'.
func segmentBoundary(s string, pos int) bool {
	if pos == 0 {
		return true
	}
	if pos >= len(s) {
		return true
	}
	return s[pos-1] == '/'
}

// millisBetween rounds a time.Duration to the nearest millisecond,
// returning at least 1 when the wall-clock delta is non-zero. Go's
// time.Duration.Milliseconds truncates toward zero, so sub-ms
// differences collapse to 0 in metrics — that's misleading for
// latency tracking. We ceil small positive durations so callers
// always see "something happened".
func millisBetween(a, b time.Time) int64 {
	if b.Before(a) {
		return 0
	}
	d := b.Sub(a)
	ms := d.Milliseconds()
	if ms == 0 && d > 0 {
		return 1
	}
	return ms
}

// rewriteJSONField replaces the value of the top-level JSON field named
// field with newValue using a literal-bytes scan. We avoid json.Marshal
// so the rest of the formatting (whitespace, key order, nested objects)
// is preserved exactly.
//
// If the body is not valid JSON, or the field is absent, the body is
// returned unchanged. We don't error — non-JSON replay is a power-user
// feature and silently no-op'ing is safer than refusing to fire.
func rewriteJSONField(body, field, newValue string) string {
	if newValue == "" {
		return body
	}
	idx := findTopLevelField(body, field)
	if idx < 0 {
		return body
	}
	// idx points at the start of the field name; scan to the colon
	// then skip past whitespace to the existing value.
	colon := strings.IndexByte(body[idx:], ':')
	if colon < 0 {
		return body
	}
	valStart := idx + colon + 1
	for valStart < len(body) && (body[valStart] == ' ' || body[valStart] == '\t') {
		valStart++
	}
	if valStart >= len(body) {
		return body
	}
	// Find the end of the existing value, respecting strings (with
	// backslash escapes) and not crossing a comma/closing brace/bracket.
	valEnd := scanJSONValueEnd(body, valStart)
	if valEnd <= valStart {
		return body
	}
	var replacement string
	if body[valStart] == '"' {
		// Preserve the quoted-string contract. We send the JSON
		// encoding of newValue so quotes and backslashes inside the
		// name are escaped correctly.
		enc, _ := json.Marshal(newValue)
		replacement = string(enc)
	} else {
		// Number, bool, null — drop in the raw bytes.
		replacement = newValue
	}
	return body[:valStart] + replacement + body[valEnd:]
}

// rewriteJSONStream flips the boolean "stream" field. *override=nil
// is a no-op. The same caveats as rewriteJSONField apply.
func rewriteJSONStream(body string, override *bool) string {
	if override == nil {
		return body
	}
	idx := findTopLevelField(body, "stream")
	if idx < 0 {
		return body
	}
	colon := strings.IndexByte(body[idx:], ':')
	if colon < 0 {
		return body
	}
	valStart := idx + colon + 1
	for valStart < len(body) && (body[valStart] == ' ' || body[valStart] == '\t') {
		valStart++
	}
	if valStart >= len(body) {
		return body
	}
	valEnd := scanJSONValueEnd(body, valStart)
	if valEnd <= valStart {
		return body
	}
	val := "false"
	if *override {
		val = "true"
	}
	return body[:valStart] + val + body[valEnd:]
}

// findTopLevelField returns the byte index in body of the start of the
// top-level field name "field", or -1 if not found. A "top-level"
// field is one whose key sits at the current depth — string contents
// and nested objects are not searched.
func findTopLevelField(body, field string) int {
	depth := 0
	inString := false
	escape := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			if depth == 1 {
				end, ok := scanString(body, i+1)
				if !ok {
					return -1
				}
				if string(body[i+1:end]) == field {
					return i
				}
				// Skip past the colon AND the value so the next
				// iteration lands on whatever comes after the
				// value (a comma, a closing brace, or whitespace).
				j := end + 1
				for j < len(body) && (body[j] == ' ' || body[j] == '	') {
					j++
				}
				if j < len(body) && body[j] == ':' {
					j++
				}
				for j < len(body) && (body[j] == ' ' || body[j] == '	') {
					j++
				}
				j = scanPastValue(body, j)
				i = j - 1
				continue
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return -1
}

// scanString reads a JSON string starting after the opening quote and
// returns the index of the closing quote. ok=false on unterminated
// string.
func scanString(body string, start int) (int, bool) {
	for i := start; i < len(body); i++ {
		c := body[i]
		if c == '\\' && i+1 < len(body) {
			i++
			continue
		}
		if c == '"' {
			return i, true
		}
	}
	return -1, false
}

// scanPastValue returns the index just past the JSON value that begins
// at valueStart (positioned on the value's first character: ", [, {
// { or a primitive).
func scanPastValue(body string, valueStart int) int {
	if valueStart >= len(body) {
		return valueStart
	}
	c := body[valueStart]
	switch c {
	case '"':
		end, ok := scanString(body, valueStart+1)
		if !ok {
			return len(body)
		}
		return end + 1
	case '{', '[':
		depth := 1
		inString := false
		escape := false
		for i := valueStart + 1; i < len(body); i++ {
			b := body[i]
			if inString {
				if escape {
					escape = false
					continue
				}
				if b == '\\' {
					escape = true
					continue
				}
				if b == '"' {
					inString = false
				}
				continue
			}
			switch b {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return len(body)
	default:
		// Primitive: scan to comma/closing brace/bracket at depth 1.
		for i := valueStart; i < len(body); i++ {
			switch body[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
		}
		return len(body)
	}
}

// scanJSONValueEnd returns the index just past the value that begins at
// valueStart. Used by the rewriters to slice out the existing value
// text.
func scanJSONValueEnd(body string, valueStart int) int {
	return scanPastValue(body, valueStart)
}

// readSSE parses an OpenAI-style SSE stream, concatenates the delta
// "content" fields into a single string, and records the time of the
// first event as the TTFT. Returns the assembled text and the elapsed
// time to the first event.
//
// The parser tolerates comment lines (":...") and the [DONE] sentinel.
// Malformed events are skipped, not fatal — an upstream that emits one
// bad line shouldn't break the whole replay.
func readSSE(r io.Reader, start time.Time) (string, time.Duration, error) {
	var (
		out      strings.Builder
		firstAt  time.Duration
		gotFirst bool
	)
	scanner := bufio.NewScanner(r)
	// Allow long lines — completion chunks can be large.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}
		if !gotFirst {
			firstAt = time.Since(start)
			gotFirst = true
		}
		// Each event is one JSON object. Extract delta.content.
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			// OpenAI includes usage on the last event of some models.
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		for _, c := range ev.Choices {
			if c.Delta.Content != "" {
				out.WriteString(c.Delta.Content)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return out.String(), firstAt, fmt.Errorf("sse scan: %w", err)
	}
	return out.String(), firstAt, nil
}

// estimatePromptTokens returns the user-role message word count * 1.3,
// rounded up. Good-enough heuristic when upstream didn't report usage.
// Zero when no user message is identifiable.
func estimatePromptTokens(body string) int {
	if body == "" {
		return 0
	}
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var v struct {
		Messages []msg `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return 0
	}
	total := 0
	for _, m := range v.Messages {
		if m.Role == "" || m.Role == "user" || m.Role == "system" {
			total += len(strings.Fields(m.Content))
		}
	}
	if total == 0 {
		return 0
	}
	// +30% to approximate sub-word tokenization overhead.
	tokens := int(float64(total) * 1.3)
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// estimateOutputTokens returns the word count of the response text,
// inflated by 1.3x to approximate sub-word tokenization. Returns 0 for
// empty responses.
func estimateOutputTokens(body string) int {
	if body == "" {
		return 0
	}
	n := len(strings.Fields(body))
	if n == 0 {
		return 0
	}
	tokens := int(float64(n) * 1.3)
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

func costFor(cfg ExecutionConfig, model string, promptTokens, outputTokens int) (float64, bool) {
	t := cfg.Pricing
	if t.IsZero() {
		t = pricing.Defaults()
	}
	return t.Cost(model, promptTokens, outputTokens)
}

// Summary returns a one-line human-readable summary of a Result,
// suitable for printing after a replay. The format is fixed-width
// where it helps parsing but readable on a terminal:
//
//	replayed id=abc123... status=200 model=demo-model-1 target=http://.../v1 ttft=82ms total=312ms in=42 out=53 cost=$0.0000
func Summary(r Result) string {
	status := fmt.Sprintf("%d", r.StatusCode)
	ttft := "-"
	if r.TTFTMillis > 0 {
		ttft = fmt.Sprintf("%dms", r.TTFTMillis)
	}
	cost := "$?"
	if r.CostUSD > 0 {
		cost = fmt.Sprintf("$%.4f", r.CostUSD)
	}
	errStr := ""
	if r.Error != "" {
		errStr = " err=" + truncateForSummary(r.Error, 60)
	}
	return fmt.Sprintf(
		"replayed id=%s status=%s model=%s target=%s ttft=%s total=%dms in=%d out=%d cost=%s stream=%t%s",
		shortID(r.RequestID),
		status,
		r.Target.Model,
		r.Target.BaseURL,
		ttft,
		r.TotalMillis,
		r.PromptTokens,
		r.OutputTokens,
		cost,
		r.Stream,
		errStr,
	)
}

// shortID truncates an ID to its first 8 hex chars for compact
// printing. Safe on empty input.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func truncateForSummary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// TruncateBody clamps a response body to n bytes, appending a marker
// when truncation occurred. Used by callers to bound what they print.
func TruncateBody(body string, n int) string {
	if n <= 0 || len(body) <= n {
		return body
	}
	return body[:n] + "\n…[truncated]"
}
