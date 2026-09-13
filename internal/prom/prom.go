// Package prom renders the per-request metrics held by a
// metrics.Recorder and a live anomaly count into the Prometheus
// text exposition format. The package is intentionally small,
// depends only on the Go standard library, and ships no third-party
// runtime deps by default (so the default `go build` of llm-top
// stays dependency-free).
//
// Design:
//
//   - One snapshot per scrape. Handler() is an http.Handler; each
//     GET calls Recorder.Snapshot() once and reads from the
//     injected anomaly source, so concurrent scrapes do not race
//     on internal state.
//   - Histograms are computed on the fly from the raw Record
//     values. The Recorder only knows about raw TTFT/Total; this
//     package projects them into the standard Prometheus default
//     bucket boundaries (0.005..10s + +Inf).
//   - Errors are bucketed into stable "kind" labels
//     (4xx, 5xx, upstream, timeout, bad_request, internal, other)
//     so the label cardinality stays bounded regardless of the
//     free-form Err message.
//   - The Go runtime metrics (process_*, go_*) are emitted so a
//     Grafana dashboard can show both the LLM traffic and the
//     proxy's own footprint on the same panel.
//
// Wiring (parent / main.go):
//
//	anomalyCh := make(chan anomaly.Anomaly, 256)
//	det := anomaly.New()
//	srv.WithPostPersistHook(func(r proxy.Request) {
//	    for _, a := range det.Evaluate(r) { anomalyCh <- a }
//	})
//	mux.Handle("/metrics", prom.Handler(rec, prom.ChanSource(anomalyCh)))
//
// Anomalies flow through a Source interface so tests can inject
// deterministic counters without spinning up a goroutine.
package prom

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/anomaly"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
)

// AnomalySource is how the prom package observes live anomaly
// counts. Implementations must be cheap to call on every scrape;
// the Channel source used in production simply reads a counter
// that the proxy increments as the detector fires.
type AnomalySource interface {
	// Snapshot returns a copy of the current (kind, severity) ->
	// count map. Safe to call concurrently.
	Snapshot() map[AnomalyKey]uint64
}

// AnomalyKey is the (kind, severity) pair used as the label
// dimension for llm_anomalies_total.
type AnomalyKey struct {
	Kind     anomaly.Kind
	Severity anomaly.Severity
}

// ChanSource returns an AnomalySource that counts anomalies
// observed on the given channel. Counts are accumulated in memory;
// the channel is drained non-blockingly on each Snapshot.
//
// Example:
//
//	anomalyCh := make(chan anomaly.Anomaly, 256)
//	srv.WithPostPersistHook(func(r proxy.Request) {
//	    for _, a := range det.Evaluate(r) {
//	        anomalyCh <- a
//	    }
//	})
//	// Drain on shutdown:
//	defer close(anomalyCh)
func ChanSource(ch <-chan anomaly.Anomaly) AnomalySource {
	return &channelSource{ch: ch, counts: map[AnomalyKey]uint64{}}
}

type channelSource struct {
	ch     <-chan anomaly.Anomaly
	mu     sync.Mutex
	counts map[AnomalyKey]uint64
}

func (c *channelSource) consume() {
	// Drain whatever's currently buffered without blocking.
	for {
		select {
		case a, ok := <-c.ch:
			if !ok {
				return
			}
			c.mu.Lock()
			c.counts[AnomalyKey{Kind: a.Kind, Severity: a.Severity}]++
			c.mu.Unlock()
		default:
			return
		}
	}
}

func (c *channelSource) Snapshot() map[AnomalyKey]uint64 {
	c.consume()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[AnomalyKey]uint64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

// StaticSource returns an AnomalySource backed by a fixed map.
// Useful in tests and for callers that pre-compute counts (e.g.
// from store.ListAnomalies on cold start).
func StaticSource(counts map[AnomalyKey]uint64) AnomalySource {
	return &staticSource{counts: counts}
}

type staticSource struct {
	counts map[AnomalyKey]uint64
}

func (s *staticSource) Snapshot() map[AnomalyKey]uint64 {
	out := make(map[AnomalyKey]uint64, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	return out
}

// DefaultBuckets are the standard Prometheus default histogram
// bucket boundaries, in seconds. +Inf is implicit (always
// appended by WriteHistogram).
var DefaultBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Handler returns an http.Handler that renders metrics.Recorder +
// AnomalySource snapshots in Prometheus text exposition format
// (Content-Type "text/plain; version=0.0.4").
//
// Recorder and src must be non-nil; an empty Recorder yields a
// well-formed scrape body with all counters at 0.
func Handler(rec *metrics.Recorder, src AnomalySource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := rec.Snapshot()
		var anomalies map[AnomalyKey]uint64
		if src != nil {
			anomalies = src.Snapshot()
		} else {
			anomalies = map[AnomalyKey]uint64{}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		Write(w, records, anomalies, DefaultBuckets)
	})
}

// key is the (model, provider) pair used as the histogram/counter
// label dimension for most LLM metrics.
type key struct {
	Model    string
	Provider string
}

// errKey is the (model, provider, kind) triple for errors.
type errKey struct {
	Model, Provider, Kind string
}

// Write renders the scrape body to w. Exposed so callers can
// integrate into a custom handler (e.g. multi-tenant wrapping).
func Write(w io.Writer, records []metrics.Record, anomalies map[AnomalyKey]uint64, buckets []float64) {
	// ----- aggregate -----
	requestCount := map[key]uint64{}
	tokenIn := map[key]uint64{}
	tokenOut := map[key]uint64{}
	costUSD := map[key]float64{}

	ttftBuckets := map[key][]uint64{}
	ttftCount := map[key]uint64{}
	ttftSum := map[key]float64{}

	latBuckets := map[key][]uint64{}
	latCount := map[key]uint64{}
	latSum := map[key]float64{}

	errCount := map[errKey]uint64{}

	for _, rec := range records {
		provider := rec.Provider
		if provider == "" {
			provider = "unknown"
		}
		model := rec.Model
		if model == "" {
			model = "unknown"
		}
		k := key{Model: model, Provider: provider}

		requestCount[k]++
		tokenIn[k] += uint64(rec.PromptTok)
		tokenOut[k] += uint64(rec.OutputTok)

		if rec.TTFT > 0 && rec.Stream {
			if _, ok := ttftBuckets[k]; !ok {
				ttftBuckets[k] = make([]uint64, len(buckets)+1) // +1 for +Inf
			}
			ttftBuckets[k] = incrementBuckets(ttftBuckets[k], buckets, rec.TTFT.Seconds())
			ttftCount[k]++
			ttftSum[k] += rec.TTFT.Seconds()
		}

		if rec.Total > 0 {
			if _, ok := latBuckets[k]; !ok {
				latBuckets[k] = make([]uint64, len(buckets)+1)
			}
			latBuckets[k] = incrementBuckets(latBuckets[k], buckets, rec.Total.Seconds())
			latCount[k]++
			latSum[k] += rec.Total.Seconds()
		}

		if rec.Err != "" || rec.Status >= 400 {
			kind := errorKind(rec.Status, rec.Err)
			errCount[errKey{Model: model, Provider: provider, Kind: kind}]++
		}
	}

	// ----- emit counters -----
	fmt.Fprintln(w, "# HELP llm_requests_total Total number of LLM requests served by the proxy.")
	fmt.Fprintln(w, "# TYPE llm_requests_total counter")
	writeCounterMap(w, "llm_requests_total", requestCount, func(k key) string {
		return fmt.Sprintf(`model=%q,provider=%q,status=%q`, k.Model, k.Provider, "all")
	})

	fmt.Fprintln(w, "# HELP llm_errors_total Total number of errored LLM requests.")
	fmt.Fprintln(w, "# TYPE llm_errors_total counter")
	writeErrMap(w, errCount)

	fmt.Fprintln(w, "# HELP llm_input_tokens_total Total input/prompt tokens (estimated if upstream did not report).")
	fmt.Fprintln(w, "# TYPE llm_input_tokens_total counter")
	writeCounterMap(w, "llm_input_tokens_total", tokenIn, func(k key) string {
		return fmt.Sprintf(`model=%q,provider=%q`, k.Model, k.Provider)
	})

	fmt.Fprintln(w, "# HELP llm_output_tokens_total Total output/completion tokens.")
	fmt.Fprintln(w, "# TYPE llm_output_tokens_total counter")
	writeCounterMap(w, "llm_output_tokens_total", tokenOut, func(k key) string {
		return fmt.Sprintf(`model=%q,provider=%q`, k.Model, k.Provider)
	})

	fmt.Fprintln(w, "# HELP llm_cost_usd_total Estimated total cost in US dollars (placeholder; pricing wiring on a follow-up).")
	fmt.Fprintln(w, "# TYPE llm_cost_usd_total counter")
	writeGaugeMap(w, "llm_cost_usd_total", costUSD, func(k key) string {
		return fmt.Sprintf(`model=%q,provider=%q`, k.Model, k.Provider)
	})

	fmt.Fprintln(w, "# HELP llm_ttft_seconds Time to first token for streaming requests, in seconds.")
	fmt.Fprintln(w, "# TYPE llm_ttft_seconds histogram")
	writeHistogram(w, "llm_ttft_seconds", ttftBuckets, ttftCount, ttftSum, buckets)

	fmt.Fprintln(w, "# HELP llm_latency_seconds Total request latency, in seconds.")
	fmt.Fprintln(w, "# TYPE llm_latency_seconds histogram")
	writeHistogram(w, "llm_latency_seconds", latBuckets, latCount, latSum, buckets)

	fmt.Fprintln(w, "# HELP llm_anomalies_total Anomalies detected by the rolling-baseline detector.")
	fmt.Fprintln(w, "# TYPE llm_anomalies_total counter")
	writeAnomalyMap(w, anomalies)

	writeRuntime(w)
}

func writeCounterMap(w io.Writer, name string, m map[key]uint64, labels func(key) string) {
	if len(m) == 0 {
		fmt.Fprintln(w, "# (no samples)")
		return
	}
	ks := make([]key, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Model != ks[j].Model {
			return ks[i].Model < ks[j].Model
		}
		return ks[i].Provider < ks[j].Provider
	})
	for _, k := range ks {
		fmt.Fprintf(w, "%s{%s} %d\n", name, labels(k), m[k])
	}
}

func writeGaugeMap(w io.Writer, name string, m map[key]float64, labels func(key) string) {
	if len(m) == 0 {
		fmt.Fprintln(w, "# (no samples)")
		return
	}
	ks := make([]key, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Model != ks[j].Model {
			return ks[i].Model < ks[j].Model
		}
		return ks[i].Provider < ks[j].Provider
	})
	for _, k := range ks {
		fmt.Fprintf(w, "%s{%s} %g\n", name, labels(k), m[k])
	}
}

func writeErrMap(w io.Writer, m map[errKey]uint64) {
	if len(m) == 0 {
		fmt.Fprintln(w, "# (no error samples)")
		return
	}
	ks := make([]errKey, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Model != ks[j].Model {
			return ks[i].Model < ks[j].Model
		}
		if ks[i].Provider != ks[j].Provider {
			return ks[i].Provider < ks[j].Provider
		}
		return ks[i].Kind < ks[j].Kind
	})
	for _, k := range ks {
		fmt.Fprintf(w, "llm_errors_total{model=%q,provider=%q,kind=%q} %d\n",
			k.Model, k.Provider, k.Kind, m[k])
	}
}

func writeHistogram(w io.Writer, name string, buckets map[key][]uint64, counts map[key]uint64, sums map[key]float64,
	defs []float64) {
	if len(buckets) == 0 {
		fmt.Fprintln(w, "# (no samples)")
		return
	}
	ks := make([]key, 0, len(buckets))
	for k := range buckets {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Model != ks[j].Model {
			return ks[i].Model < ks[j].Model
		}
		return ks[i].Provider < ks[j].Provider
	})
	for _, k := range ks {
		base := fmt.Sprintf(`model=%q,provider=%q`, k.Model, k.Provider)
		bucketVals := buckets[k]
		for i, le := range defs {
			fmt.Fprintf(w, "%s_bucket{%s,le=%q} %d\n", name, base, formatFloat(le), bucketVals[i])
		}
		fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, base, bucketVals[len(defs)])
		fmt.Fprintf(w, "%s_count{%s} %d\n", name, base, counts[k])
		fmt.Fprintf(w, "%s_sum{%s} %g\n", name, base, sums[k])
	}
}

func writeAnomalyMap(w io.Writer, m map[AnomalyKey]uint64) {
	if len(m) == 0 {
		fmt.Fprintln(w, "# (no anomalies yet)")
		return
	}
	ak := make([]AnomalyKey, 0, len(m))
	for k := range m {
		ak = append(ak, k)
	}
	sort.Slice(ak, func(i, j int) bool {
		if ak[i].Kind != ak[j].Kind {
			return string(ak[i].Kind) < string(ak[j].Kind)
		}
		return string(ak[i].Severity) < string(ak[j].Severity)
	})
	for _, k := range ak {
		fmt.Fprintf(w, "llm_anomalies_total{kind=%q,severity=%q} %d\n",
			string(k.Kind), string(k.Severity), m[k])
	}
}

// incrementBuckets adds v to every cumulative bucket whose upper
// bound is >= v. Returns the same slice. The +Inf bucket (last
// entry) is always incremented.
func incrementBuckets(b []uint64, buckets []float64, v float64) []uint64 {
	for i, le := range buckets {
		if v <= le {
			b[i]++
		}
	}
	b[len(buckets)]++ // +Inf
	return b
}

func formatFloat(f float64) string {
	// Avoid %g's lossy behavior on values like 10 (collapses to "1").
	// Use %.6f and strip trailing zeros / dot.
	s := fmt.Sprintf("%.6f", f)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}

// errorKind maps a Record's (status, error) into a stable
// short label. Keeping this closed-set prevents label cardinality
// from exploding if a Record carries a free-form upstream error
// message.
//
// Priority:
//
//	502 BadGateway         → upstream     (proxy transport error)
//	4xx (except 408)       → 4xx          (client error)
//	408 RequestTimeout     → timeout
//	5xx                    → 5xx          (server error, including 500)
//	(err message hints)    → timeout/upstream/internal
func errorKind(status int, errMsg string) string {
	if status == 0 && errMsg == "" {
		return "other"
	}
	if status == http.StatusBadGateway {
		return "upstream"
	}
	if status >= 400 && status < 500 {
		if status == http.StatusRequestTimeout {
			return "timeout"
		}
		return "4xx"
	}
	if status >= 500 && status < 600 {
		return "5xx"
	}
	msg := strings.ToLower(errMsg)
	switch {
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host"):
		return "upstream"
	case strings.Contains(msg, "build upstream") || strings.Contains(msg, "bad upstream config"):
		return "internal"
	}
	return "other"
}

// writeRuntime emits process_* / go_* metrics without importing
// prometheus/client_golang. The set is intentionally small —
// the metrics most useful for a Grafana panel showing proxy
// health alongside LLM traffic.
func writeRuntime(w io.Writer) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	fmt.Fprintln(w, "# HELP process_resident_memory_bytes Memory obtained from the OS (Sys).")
	fmt.Fprintln(w, "# TYPE process_resident_memory_bytes gauge")
	fmt.Fprintf(w, "process_resident_memory_bytes %d\n", ms.Sys)

	fmt.Fprintln(w, "# HELP process_open_fds Open file descriptors (0 on platforms without a hook).")
	fmt.Fprintln(w, "# TYPE process_open_fds gauge")
	fmt.Fprintln(w, "process_open_fds 0")

	fmt.Fprintln(w, "# HELP go_goroutines Number of goroutines that currently exist.")
	fmt.Fprintln(w, "# TYPE go_goroutines gauge")
	fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())

	fmt.Fprintln(w, "# HELP llm_top_scrape_timestamp_seconds Unix timestamp of this scrape.")
	fmt.Fprintln(w, "# TYPE llm_top_scrape_timestamp_seconds gauge")
	fmt.Fprintf(w, "llm_top_scrape_timestamp_seconds %d\n", time.Now().Unix())
}
