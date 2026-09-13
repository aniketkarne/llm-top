package ui

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/anomaly"
)

// AnomalySource is how the UI learns about live anomalies. The proxy
// process runs the TUI in-process, so callers can pass a SQLite-backed
// source (build tag `sqlite`) or an HTTP source that scrapes the proxy's
// own /metrics endpoint (which exposes llm_anomalies_total).
//
// The contract is small and forgiving: every method is best-effort, an
// error or transient failure does not affect the rest of the UI. A nil
// source means "no anomalies" and the banner stays hidden.
type AnomalySource interface {
	// Count returns the total number of anomalies observed so far.
	// Used for the "⚠ N anomalies" banner.
	Count() int
	// KindsForRequest returns the anomaly kinds observed for a given
	// request id (e.g. []anomaly.Kind{anomaly.KindTTFTSpike}).
	// Returns nil when none are recorded for that id. Used for the
	// per-row "⚠ ttft_spike" marker in the activity list.
	KindsForRequest(requestID string) []anomaly.Kind
}

// HTTPAnomalySource scrapes a Prometheus text-format endpoint
// (typically the proxy's own /metrics) and parses llm_anomalies_total
// across all (kind, severity) label combinations. It does not need
// SQLite or a store handle to work — the source-of-truth lives in
// the prom package's counter map.
//
// requestIDLookup is optional. If nil, per-row markers are disabled and
// only the banner count is populated. Wire a real lookup (e.g. over
// the SQLite store) when callers want per-row badges in addition to
// the headline count.
type HTTPAnomalySource struct {
	URL            string
	Client         *http.Client
	requestIDLookup func(string) []anomaly.Kind
}

// NewHTTPAnomalySource wraps the proxy's own /metrics endpoint. The
// caller can pass an optional per-request lookup (typically a closure
// over the SQLite store) so per-row anomaly markers render too.
func NewHTTPAnomalySource(metricsURL string, requestIDLookup func(string) []anomaly.Kind) *HTTPAnomalySource {
	return &HTTPAnomalySource{
		URL:             metricsURL,
		Client:          &http.Client{Timeout: 1500 * time.Millisecond},
		requestIDLookup: requestIDLookup,
	}
}

// Count scrapes the configured URL and sums llm_anomalies_total across
// all label combinations. Returns 0 on any error so the banner stays
// hidden rather than flashing transient errors.
func (h *HTTPAnomalySource) Count() int {
	if h == nil || h.URL == "" {
		return 0
	}
	resp, err := h.Client.Get(h.URL)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0
	}
	return parseAnomaliesFromMetrics(body)
}

// KindsForRequest delegates to the optional per-request lookup. When
// no lookup is configured, the HTTP source cannot know which anomaly
// belongs to which request id (prom exposes aggregates only), so it
// returns nil.
func (h *HTTPAnomalySource) KindsForRequest(requestID string) []anomaly.Kind {
	if h == nil || h.requestIDLookup == nil || requestID == "" {
		return nil
	}
	return h.requestIDLookup(requestID)
}

// StaticAnomalySource is a deterministic source for tests and for
// callers that already keep their own counter. It is safe for
// concurrent use because every field is read-only after construction.
type StaticAnomalySource struct {
	N    int
	ByID map[string][]anomaly.Kind
}

// Count returns the static total.
func (s *StaticAnomalySource) Count() int {
	if s == nil {
		return 0
	}
	return s.N
}

// KindsForRequest looks up the per-request anomaly kinds.
func (s *StaticAnomalySource) KindsForRequest(id string) []anomaly.Kind {
	if s == nil || s.ByID == nil {
		return nil
	}
	return s.ByID[id]
}

// anomalyBadge returns the short badge string used inline next to a
// request row in the activity list, and once in the TUI header. The
// format is kept tight ("⚠ ttft_spike") so it fits a single column in
// the standard 100-wide dashboard.
//
// Empty input or unknown kinds fall back to a generic "anomaly" badge
// so the row still gets flagged.
func anomalyBadge(k anomaly.Kind) string {
	switch k {
	case "":
		return "⚠ anomaly"
	case anomaly.KindTTFTSpike:
		return "⚠ ttft_spike"
	case anomaly.KindLatencySpike:
		return "⚠ latency_spike"
	case anomaly.KindTokenExplosion:
		return "⚠ token_explosion"
	case anomaly.KindRepeatedPrompt:
		return "⚠ repeated_prompt"
	case anomaly.KindErrorBurst:
		return "⚠ error_burst"
	case anomaly.KindLargeContext:
		return "⚠ large_context"
	case anomaly.KindProviderDegrade:
		return "⚠ provider_degrade"
	}
	// Unknown kind: still surface the raw name so operators see what
	// the detector labeled without needing to ship a code change.
	return "⚠ " + string(k)
}

// parseAnomaliesFromMetrics walks a Prometheus text-format scrape body
// and sums llm_anomalies_total across every (kind, severity) label
// combination. The Prometheus client format only writes one sample per
// line for counter maps, so a simple line scan is enough; we don't pull
// in a full parser.
//
// Lines that are comments ("#"), empty, or non-numeric are skipped so
// unrelated counters ("llm_requests_total", "llm_errors_total") do
// not contribute.
//
// Examples of lines we accept:
//
//	llm_anomalies_total{kind="ttft_spike",severity="warn"} 1
//	llm_anomalies_total{kind="latency_spike",severity="warn"} 1
//
// Returns 0 when the body has no llm_anomalies_total samples (e.g.
// before any anomaly has fired).
func parseAnomaliesFromMetrics(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	total := 0
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	// Some Prometheus bodies exceed the default 64KiB scanner buffer
	// when the proxy has been running for a while. Bump to 1 MiB to
	// match io.LimitReader used by HTTPAnomalySource.Count.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "llm_anomalies_total") {
			continue
		}
		// Expect: llm_anomalies_total{...} <value>
		i := strings.LastIndex(line, " ")
		if i < 0 || i == len(line)-1 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
		if err != nil {
			continue
		}
		// Counters are non-negative integers; clamp to [0, max-int]
		// so a runaway sample never overflows.
		if v < 0 {
			continue
		}
		total += int(v)
		if total < 0 {
			total = 1<<31 - 1
		}
	}
	return total
}

// bannerText formats the "⚠ N anomalies" banner shown at the top of
// the TUI when the count is non-zero. Empty string when count == 0 so
// callers can blindly concatenate.
func bannerText(count int) string {
	if count <= 0 {
		return ""
	}
	return fmt.Sprintf(" ⚠ %d %s ", count, pluralize(count, "anomaly", "anomalies"))
}

// pluralize is a tiny helper that keeps bannerText honest about
// singular vs plural.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
