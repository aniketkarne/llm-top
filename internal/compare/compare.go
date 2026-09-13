// Package compare re-fires a captured proxy.Request to several upstream
// targets in parallel and aggregates per-target metrics into a single
// result suitable for rendering as a side-by-side table.
//
// The package is intentionally narrow: it owns the dispatch loop and the
// Target/Row/CompareResult shapes, but it does NOT parse SSE or compute
// cost — those live in internal/replay and internal/pricing and are
// reached by going through replay.Execute. We never duplicate parsing.
//
// Concurrency model: by default every target runs in parallel. Pass
// Concurrency > 0 to bound the fan-out to N goroutines. A target that
// fails (transport error, non-2xx, parse error, timeout) does NOT abort
// the others — its Row is populated with an Error string and the
// remaining Rows report normally.
package compare

import (
	"context"
	"sync"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/pricing"
	"github.com/aniketkarne-com/llm-top/internal/proxy"
	"github.com/aniketkarne-com/llm-top/internal/replay"
)

// MaxResponseBodyBytes caps the per-row ResponseBody we keep in memory
// after a replay. Full bodies can be megabytes; the rendered table only
// needs the first 4KB, and the excerpts section further truncates to
// 200 chars. We pass a generous 4KB so the excerpt section is never
// working from a string that's already been trimmed mid-token.
const MaxResponseBodyBytes = 4096

// MaxExcerptBytes is the maximum number of response-body characters we
// show in the per-target "excerpts" section after the table. Past 200
// the row is suffixed with a "…" so the line stays one terminal line.
const MaxExcerptBytes = 200

// Target describes one upstream to send the captured request to.
// Name is the column header in the rendered table. BaseURL is required.
// APIKey is sent as a Bearer token; empty for local/Ollama. Provider,
// Model, and ExtraHeaders are optional overrides.
type Target struct {
	Name         string
	BaseURL      string
	APIKey       string
	Provider     string
	Model        string
	ExtraHeaders map[string]string
}

// Row is one per-target comparison result. Mirrors the columns the
// rendered table needs plus enough metadata for callers to drill in
// (BaseURL, ResponseBody for excerpts).
type Row struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	BaseURL       string `json:"base_url"`
	StatusCode    int    `json:"status_code"`
	Stream        bool   `json:"stream"`
	TTFTMillis    int64  `json:"ttft_ms"`
	TotalMillis   int64  `json:"total_ms"`
	PromptTokens  int    `json:"prompt_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	CostUSD       float64 `json:"cost_usd"`
	CostKnown     bool   `json:"cost_known"`
	ResponseBody  string `json:"response_body,omitempty"`
	Error         string `json:"error,omitempty"`
}

// CompareResult is the per-call aggregate: which captured request we
// ran and one Row per target, in the same order the caller passed
// targets in (so the rendered table is stable).
type CompareResult struct {
	CapturedID string `json:"captured_id"`
	Rows       []Row  `json:"rows"`
}

// Options configures Run. Zero values are sensible: a zero Timeout
// falls back to 30s; Concurrency <= 0 means "all targets at once".
type Options struct {
	// Timeout is per-target. Defaults to 30s when zero.
	Timeout time.Duration
	// Concurrency caps parallel targets. <= 0 means len(Targets).
	Concurrency int
	// Pricing is forwarded to replay.Execute so cost is computed the
	// same way across runs. Zero falls back to pricing.Defaults().
	Pricing pricing.Table
}

// Run re-fires captured against every target in parallel and returns a
// CompareResult with one Row per target. The returned Rows slice is in
// the same order as targets — no need for the caller to sort.
//
// The function never returns an error: a per-target failure is encoded
// in Row.Error (and possibly Row.StatusCode=0), not as a function-level
// return. This keeps the renderer straightforward and makes "compare"
// trivially safe to chain.
func Run(ctx context.Context, captured proxy.Request, targets []Target, opts Options) CompareResult {
	res := CompareResult{CapturedID: captured.ID, Rows: make([]Row, len(targets))}
	if len(targets) == 0 {
		return res
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 || concurrency > len(targets) {
		concurrency = len(targets)
	}

	// Bounded fan-out: a semaphore channel of size `concurrency`
	// gates how many goroutines may call replay.Execute at once.
	// We always spawn one goroutine per target so they can all be
	// in flight simultaneously when concurrency permits; the
	// semaphore is what enforces the cap.
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, t := range targets {
		i, t := i, t
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res.Rows[i] = executeOne(ctx, captured, t, timeout, opts.Pricing)
		}()
	}
	wg.Wait()
	return res
}

// executeOne wraps replay.Execute for a single target and shapes the
// replay.Result into a compare.Row. ResponseBody is truncated to 4KB.
func executeOne(ctx context.Context, captured proxy.Request, t Target, timeout time.Duration, ptab pricing.Table) Row {
	if t.BaseURL == "" {
		return Row{
			Name:    t.Name,
			Model:   effectiveModel(captured, t),
			BaseURL: "",
			Error:   "target base URL is empty (use --target name=url|key, or --local)",
		}
	}
	cfg := replay.ExecutionConfig{
		UpstreamBaseURL: t.BaseURL,
		UpstreamAPIKey:  t.APIKey,
		Model:           t.Model,
		Headers:         t.ExtraHeaders,
		Timeout:         timeout,
		Pricing:         ptab,
	}
	rr := replay.Execute(ctx, captured, cfg)

	row := Row{
		Name:         t.Name,
		Model:        rr.Target.Model,
		Provider:     rr.Target.Provider,
		BaseURL:      rr.Target.BaseURL,
		StatusCode:   rr.StatusCode,
		Stream:       rr.Stream,
		TTFTMillis:   rr.TTFTMillis,
		TotalMillis:  rr.TotalMillis,
		PromptTokens: rr.PromptTokens,
		OutputTokens: rr.OutputTokens,
		CostUSD:      rr.CostUSD,
		CostKnown:    rr.CostUSD > 0 || rr.Error == "",
		ResponseBody: truncateBytes(rr.ResponseBody, MaxResponseBodyBytes),
		Error:        rr.Error,
	}
	// CostKnown must reflect whether pricing.Defaults() actually had
	// a rate for the model we ran. We re-check the table here so the
	// renderer can show "$?" even when cost is non-zero for an
	// unrelated reason (e.g. user-supplied pricing override).
	tab := ptab
	if tab.IsZero() {
		tab = pricing.Defaults()
	}
	if _, ok := tab.Cost(row.Model, 1, 1); !ok {
		// Whether cost came out zero or positive, if the model is
		// unknown we want the renderer to show "$?".
		row.CostKnown = false
		row.CostUSD = 0
	}
	// Provider override: the user can pin a provider label even when
	// the upstream URL doesn't pattern-match. The replay layer sets
	// Provider via proxy.ProviderFromUpstream; we override here so
	// the rendered table reflects the user's intent.
	if t.Provider != "" {
		row.Provider = t.Provider
	}
	return row
}

// effectiveModel returns the model the row will use. Mirrors
// replay.EffectiveModel but takes a Target instead of ExecutionConfig.
func effectiveModel(captured proxy.Request, t Target) string {
	if t.Model != "" {
		return t.Model
	}
	return captured.Model
}

// truncateBytes clamps a string to n bytes, appending "…" when
// truncated. Safe on empty input.
func truncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Excerpt returns the first MaxExcerptBytes characters of the response
// body with a trailing "…" when truncated. It's exported so the
// renderer can be exercised directly in tests, and so callers (the
// CLI) can render the same line the table ends with.
func Excerpt(body string) string {
	return truncateBytes(body, MaxExcerptBytes)
}
