package replay

import (
	"fmt"
	"strings"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// CurlOptions configures CurlCommand. Zero values are sensible: a
// missing APIKey causes the renderer to emit $LLMTOP_API_KEY so the
// user can paste-and-run without leaking secrets.
type CurlOptions struct {
	// UpstreamBaseURL overrides the destination if non-empty. When
	// empty, the captured request's Upstream is used. The renderer
	// always appends the captured Path to the resolved base URL.
	UpstreamBaseURL string

	// APIKey, when non-empty, replaces the literal $LLMTOP_API_KEY
	// placeholder in the rendered Authorization header.
	APIKey string

	// RevealKey, when true, substitutes a non-empty APIKey verbatim
	// into the command. When false (the default), the command always
	// contains $LLMTOP_API_KEY so the user can paste it into another
	// terminal without leaking secrets. RevealKey with an empty
	// APIKey is a no-op — $LLMTOP_API_KEY is still emitted.
	RevealKey bool

	// ExtraHeaders, when non-empty, are appended as additional -H
	// flags. Values are sent verbatim.
	ExtraHeaders map[string]string

	// Model, when non-empty, rewrites the "model" field of the JSON
	// body in the rendered -d payload.
	Model string

	// Stream, when non-nil, rewrites the "stream" field of the JSON
	// body in the rendered -d payload.
	Stream *bool
}

// CurlCommand renders a ready-to-paste curl invocation that replays a
// captured request. The output is suitable for piping into `sh` or for
// copying into another terminal; by default the Authorization header
// uses $LLMTOP_API_KEY rather than embedding the literal key.
//
// The path defaults to /v1/chat/completions when the captured request
// doesn't carry one (so curl is useful even when the user only has a
// store row ID).
func CurlCommand(captured proxy.Request, opts CurlOptions) string {
	base := opts.UpstreamBaseURL
	if base == "" {
		base = captured.Upstream
	}
	if base == "" {
		base = "https://api.openai.com"
	}
	p := captured.Path
	if p == "" {
		p = "/v1/chat/completions"
	}
	url := joinURL(base, p)

	body := captured.RequestBody
	if body != "" {
		body = rewriteJSONField(body, "model", opts.Model)
		body = rewriteJSONStream(body, opts.Stream)
	}

	auth := "$LLMTOP_API_KEY"
	if opts.RevealKey && opts.APIKey != "" {
		auth = opts.APIKey
	}

	var b strings.Builder
	fmt.Fprintf(&b, "curl -sS -X POST '%s'", url)
	fmt.Fprintf(&b, " \\\n  -H 'Authorization: Bearer %s'", auth)
	fmt.Fprintf(&b, " \\\n  -H 'Content-Type: application/json'")
	keys := make([]string, 0, len(opts.ExtraHeaders))
	for k := range opts.ExtraHeaders {
		keys = append(keys, k)
	}
	// Stable order so output is deterministic for tests/snapshots.
	sortStrings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " \\\n  -H '%s: %s'", k, opts.ExtraHeaders[k])
	}
	if body != "" {
		// Single-quote the body so most shell metacharacters pass
		// through. We replace embedded single quotes with the
		// standard '"'"' escape so the body remains a valid shell
		// argument.
		fmt.Fprintf(&b, " \\\n  -d '%s'", shellQuote(body))
	}
	return b.String()
}

// shellQuote escapes a body for use inside single quotes in a POSIX
// shell. Embedding a single quote requires us to close, insert a
// literal single quote (escaped via double quotes), then reopen.
func shellQuote(s string) string {
	return strings.ReplaceAll(s, "'", `'"'"'`)
}

// sortStrings is a small helper to keep the curl header order stable
// without importing "sort" at the package top.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
