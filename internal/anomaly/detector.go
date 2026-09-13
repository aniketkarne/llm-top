// Detector owns the rolling baselines that power anomaly detection.
// It is the proxy's hot path: Evaluate is called once per captured
// request and returns the anomalies that the request triggered.
//
// The Detector is safe for concurrent use. All mutable state sits
// behind a single sync.Mutex. The hot path (Evaluate) holds the
// mutex for the duration of one Evaluate call; under realistic
// traffic (tens of req/s) the contention is negligible. A sharded
// design would be premature.
//
// Rolling statistics are stored per (model, metric) pair in ring
// buffers of size WindowSize. Lazily allocated on first sight. The
// current "spike" tests compare the new value to a nearest-rank
// percentile over the buffer BEFORE the new value is appended, so
// the very first request never triggers a spike (no baseline yet).
//
// Prompt-hash hits and error bursts use a fixed-size ring buffer of
// recent request metadata. The window for repeated-prompt detection
// is a time window (RepeatedPromptWindow); the window for error
// bursts is a count window (ErrorBurstWindow). These are different
// semantics on purpose — see detector comments.
package anomaly

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// Detector is the rolling-baseline anomaly detector. Configure it
// before use (the defaults set by New are sensible for OpenAI-class
// traffic; tune them via the public fields for other workloads).
type Detector struct {
	// Multipliers are applied as "current > multiplier x baseline".
	TTFTMultiplier      float64
	LatencyMultiplier   float64
	TokenMultiplier     float64
	ProviderDegradeMult float64

	// Repeated-prompt detector configuration.
	RepeatedPromptCount  int
	RepeatedPromptWindow time.Duration

	// Error-burst detector configuration.
	ErrorBurstThreshold float64
	ErrorBurstWindow    int

	// Large-context absolute threshold.
	LargeContextTokens int

	// Per-(model,metric) ring buffer size for rolling statistics.
	WindowSize int

	// Clock is injectable for tests. Defaults to time.Now.
	Clock func() time.Time

	mu sync.Mutex

	// Per-model streaming-TTFT ring buffer. Only streaming requests
	// contribute; non-streaming requests always have TTFTMillis==0
	// and would skew the baseline to zero.
	ttftByModel map[string]*ringBuf

	// Per-model total-latency ring buffer (streaming + JSON).
	totalByModel map[string]*ringBuf

	// Per-model output-token ring buffer.
	outTokByModel map[string]*ringBuf

	// Per-(model,provider) error ring buffer: each entry records
	// 1 for error, 0 for success. Used by both ErrorBurst and
	// ProviderDegrade detectors.
	errByProvider map[providerKey]*ringBuf

	// Per-prompt-hash ring buffer of (id, timestamp). Eviction
	// happens lazily during Evaluate.
	recentByHash map[string]*hashRing

	// Last-fire index per (model,provider) for ErrorBurst suppression.
	lastFireByPair map[providerKey]int

	// Recent anomalies, newest first. Bounded; the ring wraps.
	recent *ringBufAnomaly
}

// providerKey is a (model, provider) pair. ProviderDegrade compares
// one provider's error rate against the average of others.
type providerKey struct {
	Model    string
	Provider string
}

// ringBuf is a small, allocation-free fixed-size ring buffer of
// float64 values. All access is guarded by Detector.mu.
type ringBuf struct {
	data []float64
	head int // next write index
	size int // current number of valid entries (<= cap)
	cap  int // max entries
}

// newRing returns an empty ring buffer with the given capacity. cap
// must be > 0; callers should validate.
func newRing(cap int) *ringBuf {
	if cap <= 0 {
		cap = 1
	}
	return &ringBuf{
		data: make([]float64, cap),
		cap:  cap,
	}
}

// push appends v, overwriting the oldest entry when full.
func (r *ringBuf) push(v float64) {
	if r.cap == 0 {
		return
	}
	r.data[r.head] = v
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}

// values returns the valid entries in chronological order (oldest
// first, newest last). Allocates a fresh slice.
func (r *ringBuf) values() []float64 {
	if r.size == 0 {
		return nil
	}
	out := make([]float64, r.size)
	// Oldest entry is at (head - size) mod cap.
	start := (r.head - r.size + r.cap) % r.cap
	for i := 0; i < r.size; i++ {
		out[i] = r.data[(start+i)%r.cap]
	}
	return out
}

// nearestRank returns the nearest-rank percentile (1-100) of the
// values in v. If v is empty, returns 0.
//
// "Nearest rank" means: rank = ceil(p/100 * n), then pick the value
// at that 1-indexed position in the sorted slice. This is the
// textbook definition; it never extrapolates between data points
// and is easy to reason about. For our small windows (default 100)
// any smoother interpolation would be over-engineering.
func nearestRank(v []float64, p float64) float64 {
	n := len(v)
	if n == 0 {
		return 0
	}
	if p <= 0 {
		p = 1
	}
	if p > 100 {
		p = 100
	}
	rank := int(math.Ceil(p/100*float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	cp := make([]float64, n)
	copy(cp, v)
	sort.Float64s(cp)
	return cp[rank-1]
}

// mean returns the arithmetic mean of v. Returns 0 for an empty slice.
func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

// hashEntry records one occurrence of a prompt_hash. We keep the
// request id (for the Anomaly.RequestID field on trigger) and the
// timestamp (for the sliding-window eviction).
type hashEntry struct {
	id string
	at time.Time
}

// hashRing is a bounded per-hash history. The cap is set from
// RepeatedPromptCount * 4 so we always have room for "count events
// in window" without unbounded growth; old entries past the window
// are pruned during push.
type hashRing struct {
	entries []hashEntry
	cap     int
}

func newHashRing(cap int) *hashRing {
	if cap <= 0 {
		cap = 1
	}
	return &hashRing{cap: cap}
}

// push records (id, t) and prunes entries older than window. Returns
// the current count of entries within the window after pruning.
func (h *hashRing) push(id string, t time.Time, window time.Duration) int {
	cutoff := t.Add(-window)
	// Drop head entries older than cutoff.
	i := 0
	for i < len(h.entries) && h.entries[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		h.entries = h.entries[i:]
	}
	h.entries = append(h.entries, hashEntry{id: id, at: t})
	// Enforce cap (defensive; shouldn't fire under sane configs).
	if len(h.entries) > h.cap {
		h.entries = h.entries[len(h.entries)-h.cap:]
	}
	return len(h.entries)
}

// ringBufAnomaly is a bounded []Anomaly. Newest entries are appended
// at the tail; Recent slices from the tail backwards.
type ringBufAnomaly struct {
	data []Anomaly
	cap  int
}

func newAnomalyRing(cap int) *ringBufAnomaly {
	if cap <= 0 {
		cap = 1
	}
	return &ringBufAnomaly{cap: cap}
}

func (r *ringBufAnomaly) push(a Anomaly) {
	r.data = append(r.data, a)
	if len(r.data) > r.cap {
		// Drop oldest.
		r.data = r.data[len(r.data)-r.cap:]
	}
}

func (r *ringBufAnomaly) recent(n int) []Anomaly {
	if n <= 0 || len(r.data) == 0 {
		return nil
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	out := make([]Anomaly, n)
	copy(out, r.data[len(r.data)-n:])
	// Newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// New returns a Detector with the documented defaults. Callers may
// then tweak any public field before the first Evaluate call.
func New() *Detector {
	return &Detector{
		TTFTMultiplier:       3.0,
		LatencyMultiplier:    3.0,
		TokenMultiplier:      3.0,
		ProviderDegradeMult:  3.0,
		RepeatedPromptCount:  3,
		RepeatedPromptWindow: 60 * time.Second,
		ErrorBurstThreshold:  0.25,
		ErrorBurstWindow:     20,
		LargeContextTokens:   32000,
		WindowSize:           100,
		Clock:                func() time.Time { return time.Now().UTC() },
		ttftByModel:          make(map[string]*ringBuf),
		totalByModel:         make(map[string]*ringBuf),
		outTokByModel:        make(map[string]*ringBuf),
		errByProvider:        make(map[providerKey]*ringBuf),
		recentByHash:         make(map[string]*hashRing),
		lastFireByPair:       make(map[providerKey]int),
		recent:               newAnomalyRing(1000),
	}
}

// Evaluate inspects a freshly-persisted Request and returns the
// anomalies it triggered. Always safe for concurrent use.
//
// Evaluate is the single entrypoint that updates all rolling
// baselines. Internal callers grab Detector.mu once at the top and
// release at the bottom so all baseline updates for one request
// appear atomic to other Evaluate calls.
//
// The detectors run in this order:
//
//  1. LargeContext   (absolute threshold; always checked)
//  2. TTFTSpike      (only when streaming and WindowSize filled)
//  3. LatencySpike   (always when WindowSize filled)
//  4. TokenExplosion (per-model mean; needs >= 5 samples to fire)
//  5. RepeatedPrompt (time window; fires as soon as threshold met)
//  6. ErrorBurst     (count window; fires after ErrorBurstWindow reqs)
//  7. ProviderDegrade (per-(model,provider) error rate)
//
// A single request may trigger multiple anomalies (e.g. a streaming
// spike on a repeated prompt is two anomalies).
func (d *Detector) Evaluate(r proxy.Request) []Anomaly {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	var out []Anomaly

	// 1. LargeContext — absolute threshold, always checked.
	if r.PromptTokens > d.largeContextTokens() {
		out = append(out, Anomaly{
			Kind:       KindLargeContext,
			Severity:   SeverityWarn,
			Message:    fmt.Sprintf("large context: %d input tokens (threshold %d)", r.PromptTokens, d.LargeContextTokens),
			DetectedAt: now,
			RequestID:  r.ID,
			Baseline:   fmt.Sprintf("%d", d.LargeContextTokens),
			Observed:   fmt.Sprintf("%d", r.PromptTokens),
			Extra:      map[string]string{"model": r.Model},
		})
	}

	// Lazily allocate per-model ring buffers.
	if _, ok := d.ttftByModel[r.Model]; !ok && r.Model != "" {
		d.ttftByModel[r.Model] = newRing(d.WindowSize)
	}
	if _, ok := d.totalByModel[r.Model]; !ok && r.Model != "" {
		d.totalByModel[r.Model] = newRing(d.WindowSize)
	}
	if _, ok := d.outTokByModel[r.Model]; !ok && r.Model != "" {
		d.outTokByModel[r.Model] = newRing(d.WindowSize)
	}

	// 2. TTFTSpike — streaming only. Compare against the buffer
	// BEFORE pushing the current value, so the very first request
	// can never trigger a spike (no baseline).
	if r.Stream && r.TTFTMillis > 0 && r.Model != "" {
		buf := d.ttftByModel[r.Model]
		baseline := nearestRank(buf.values(), 95)
		if buf.size >= 10 && float64(r.TTFTMillis) > d.ttftMult()*baseline {
			mult := 0.0
			if baseline > 0 {
				mult = float64(r.TTFTMillis) / baseline
			}
			out = append(out, Anomaly{
				Kind:       KindTTFTSpike,
				Severity:   SeverityWarn,
				Message:    fmt.Sprintf("ttft %sms on %s (baseline %sms, x%.1f)", humanInt(r.TTFTMillis), r.Model, humanInt(int64(baseline)), mult),
				DetectedAt: now,
				RequestID:  r.ID,
				Baseline:   fmt.Sprintf("%dms", int64(baseline)),
				Observed:   fmt.Sprintf("%dms", r.TTFTMillis),
				Extra:      map[string]string{"model": r.Model, "ratio": fmt.Sprintf("%.2f", mult)},
			})
		}
	}

	// 3. LatencySpike — always when we have a baseline.
	if r.TotalMillis > 0 && r.Model != "" {
		buf := d.totalByModel[r.Model]
		baseline := nearestRank(buf.values(), 95)
		if buf.size >= 10 && float64(r.TotalMillis) > d.latencyMult()*baseline {
			mult := 0.0
			if baseline > 0 {
				mult = float64(r.TotalMillis) / baseline
			}
			out = append(out, Anomaly{
				Kind:       KindLatencySpike,
				Severity:   SeverityWarn,
				Message:    fmt.Sprintf("total %sms on %s (baseline %sms, x%.1f)", humanInt(r.TotalMillis), r.Model, humanInt(int64(baseline)), mult),
				DetectedAt: now,
				RequestID:  r.ID,
				Baseline:   fmt.Sprintf("%dms", int64(baseline)),
				Observed:   fmt.Sprintf("%dms", r.TotalMillis),
				Extra:      map[string]string{"model": r.Model, "ratio": fmt.Sprintf("%.2f", mult)},
			})
		}
	}

	// 4. TokenExplosion — compare against the rolling MEAN (not p95)
	// because output-token variance is high; p95 is too sensitive.
	if r.OutputTokens > 0 && r.Model != "" {
		buf := d.outTokByModel[r.Model]
		m := mean(buf.values())
		if buf.size >= 5 && float64(r.OutputTokens) > d.tokenMult()*m {
			mult := 0.0
			if m > 0 {
				mult = float64(r.OutputTokens) / m
			}
			out = append(out, Anomaly{
				Kind:       KindTokenExplosion,
				Severity:   SeverityInfo,
				Message:    fmt.Sprintf("%s output tokens on %s (baseline %s, x%.1f)", humanInt(int64(r.OutputTokens)), r.Model, humanInt(int64(m)), mult),
				DetectedAt: now,
				RequestID:  r.ID,
				Baseline:   fmt.Sprintf("%d", int64(m)),
				Observed:   fmt.Sprintf("%d", r.OutputTokens),
				Extra:      map[string]string{"model": r.Model, "ratio": fmt.Sprintf("%.2f", mult)},
			})
		}
	}

	// 5. RepeatedPrompt — sliding window keyed by prompt hash.
	if r.PromptHash != "" {
		key := r.PromptHash
		if _, ok := d.recentByHash[key]; !ok {
			// Cap at 16 to bound memory. RepeatedPromptCount default
			// is 3; this gives 5x headroom for window eviction
			// before triggering a prune.
			d.recentByHash[key] = newHashRing(16)
		}
		count := d.recentByHash[key].push(r.ID, now, d.repeatedPromptWindow())
		if count >= d.repeatedPromptCount() && count == d.repeatedPromptCount() {
			out = append(out, Anomaly{
				Kind:       KindRepeatedPrompt,
				Severity:   SeverityInfo,
				Message:    fmt.Sprintf("prompt %s... fired %d times in %s", shortHash(r.PromptHash), count, d.repeatedPromptWindow()),
				DetectedAt: now,
				RequestID:  r.ID,
				Baseline:   fmt.Sprintf("count<%d", d.repeatedPromptCount()),
				Observed:   fmt.Sprintf("count=%d", count),
				Extra: map[string]string{
					"prompt_hash": r.PromptHash,
					"count":       fmt.Sprintf("%d", count),
					"window":      d.repeatedPromptWindow().String(),
				},
			})
		}
	}

	// 6. ErrorBurst — fraction of errors in the last
	// ErrorBurstWindow requests (any model). Threshold is inclusive.
	// We snapshot the buffer BEFORE pushing the current value so the
	// transition from "below threshold" to "above threshold" is
	// detectable on the first error in a fresh window. To avoid
	// alert spam, we also track per-pair "last fire" timestamps and
	// suppress re-firing within half the window of the last one —
	// enough breathing room for a real burst to settle, short enough
	// to fire again if the rate keeps climbing.
	errValue := 0.0
	if r.Error != "" || r.StatusCode >= 400 {
		errValue = 1.0
	}
	pk := providerKey{Model: r.Model, Provider: r.Provider}
	if _, ok := d.errByProvider[pk]; !ok {
		d.errByProvider[pk] = newRing(d.errorBurstWindow())
	}
	errBuf := d.errByProvider[pk]

	const minSamples = 5
	if errBuf.size >= minSamples {
		vals := errBuf.values()
		if len(vals) > d.errorBurstWindow() {
			vals = vals[len(vals)-d.errorBurstWindow():]
		}
		prevSum := 0.0
		for _, v := range vals {
			prevSum += v
		}
		prevRate := prevSum / float64(len(vals))
		newSum := prevSum + errValue
		newRate := newSum / float64(len(vals))
		// Fire when EITHER the rate is rising across threshold OR the
		// current rate is high enough to be alarming AND we haven't
		// fired for a full window. The second clause catches
		// sustained-burst cases (all-errors from a fresh start) where
		// prevRate is always > threshold so the rising-edge test
		// would never trigger. The full-window cool-down prevents
		// alert spam while still allowing re-fire on a new burst
		// event after the current one settles.
		rising := newRate > d.errorBurstThreshold() && prevRate <= d.errorBurstThreshold()
		sustained := newRate > d.errorBurstThreshold() && errBuf.size-d.lastFireByPair[pk] >= d.errorBurstWindow()
		if rising || sustained {
			if last, ok := d.lastFireByPair[pk]; !ok || errBuf.size-last >= d.errorBurstWindow() {
				out = append(out, Anomaly{
					Kind:       KindErrorBurst,
					Severity:   SeverityAlert,
					Message:    fmt.Sprintf("%.0f%% errors on %s/%s in last %d requests", newRate*100, r.Model, r.Provider, len(vals)),
					DetectedAt: now,
					RequestID:  r.ID,
					Baseline:   fmt.Sprintf("%.0f%%", d.errorBurstThreshold()*100),
					Observed:   fmt.Sprintf("%.0f%% (%d/%d)", newRate*100, int(newSum), len(vals)),
					Extra: map[string]string{
						"model":    r.Model,
						"provider": r.Provider,
						"errors":   fmt.Sprintf("%d", int(newSum)),
						"total":    fmt.Sprintf("%d", len(vals)),
					},
				})
				d.lastFireByPair[pk] = errBuf.size
			}
		}
	}
	// Push AFTER the check so the just-evaluated request never
	// poisons its own previous-state.
	errBuf.push(errValue)

	// 7. ProviderDegrade — compare this (model,provider) error rate
	// to the average rate across other providers for the same model.
	if r.Model != "" && r.Provider != "" {
		thisRate, thisN := providerRateAndN(d.errByProvider, pk)
		var (
			others     []float64
			otherCount int
		)
		for k, buf := range d.errByProvider {
			if k.Model != r.Model || k.Provider == r.Provider {
				continue
			}
			if buf.size < 5 {
				continue
			}
			vals := buf.values()
			s := 0.0
			for _, v := range vals {
				s += v
			}
			others = append(others, s/float64(len(vals)))
			otherCount++
		}
		if thisN >= 5 && otherCount >= 1 {
			otherMean := mean(others)
			// Require this rate to be significantly above other
			// providers AND the other providers to NOT be in a
			// widespread outage (would compare 80% vs 75%).
			if otherMean < 0.5 && thisRate > d.providerDegradeMult()*otherMean && thisRate > 0.1 {
				out = append(out, Anomaly{
					Kind:       KindProviderDegrade,
					Severity:   SeverityAlert,
					Message:    fmt.Sprintf("%s/%s error rate %.0f%% vs other providers %.0f%% (x%.1f)", r.Model, r.Provider, thisRate*100, otherMean*100, thisRate/math.Max(otherMean, 0.001)),
					DetectedAt: now,
					RequestID:  r.ID,
					Baseline:   fmt.Sprintf("%.0f%%", otherMean*100),
					Observed:   fmt.Sprintf("%.0f%%", thisRate*100),
					Extra: map[string]string{
						"model":      r.Model,
						"provider":   r.Provider,
						"this_rate":  fmt.Sprintf("%.4f", thisRate),
						"other_rate": fmt.Sprintf("%.4f", otherMean),
					},
				})
			}
		}
	}

	// Now commit all baselines for this request. Order matters only
	// for TTFT (we already compared above); commit AFTER the spike
	// test so the just-evaluated request never poisons its own
	// baseline.
	if r.Stream && r.TTFTMillis > 0 && r.Model != "" {
		d.ttftByModel[r.Model].push(float64(r.TTFTMillis))
	}
	if r.TotalMillis > 0 && r.Model != "" {
		d.totalByModel[r.Model].push(float64(r.TotalMillis))
	}
	if r.OutputTokens > 0 && r.Model != "" {
		d.outTokByModel[r.Model].push(float64(r.OutputTokens))
	}

	for _, a := range out {
		d.recent.push(a)
	}

	return out
}

// Recent returns the n most recent anomalies across all Kinds,
// newest first. Returns nil when n <= 0 or no anomalies have fired.
func (d *Detector) Recent(n int) []Anomaly {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.recent.recent(n)
}

// now returns the current time according to the injectable Clock,
// falling back to time.Now if Clock is nil (defensive — New sets a
// default).
func (d *Detector) now() time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	return time.Now().UTC()
}

// --- accessors with safe defaults; called with d.mu held. ---

func (d *Detector) largeContextTokens() int {
	if d.LargeContextTokens > 0 {
		return d.LargeContextTokens
	}
	return 32000
}

func (d *Detector) ttftMult() float64 {
	if d.TTFTMultiplier > 0 {
		return d.TTFTMultiplier
	}
	return 3.0
}

func (d *Detector) latencyMult() float64 {
	if d.LatencyMultiplier > 0 {
		return d.LatencyMultiplier
	}
	return 3.0
}

func (d *Detector) tokenMult() float64 {
	if d.TokenMultiplier > 0 {
		return d.TokenMultiplier
	}
	return 3.0
}

func (d *Detector) providerDegradeMult() float64 {
	if d.ProviderDegradeMult > 0 {
		return d.ProviderDegradeMult
	}
	return 3.0
}

func (d *Detector) repeatedPromptCount() int {
	if d.RepeatedPromptCount > 0 {
		return d.RepeatedPromptCount
	}
	return 3
}

func (d *Detector) repeatedPromptWindow() time.Duration {
	if d.RepeatedPromptWindow > 0 {
		return d.RepeatedPromptWindow
	}
	return 60 * time.Second
}

func (d *Detector) errorBurstThreshold() float64 {
	if d.ErrorBurstThreshold > 0 {
		return d.ErrorBurstThreshold
	}
	return 0.25
}

func (d *Detector) errorBurstWindow() int {
	if d.ErrorBurstWindow > 0 {
		return d.ErrorBurstWindow
	}
	return 20
}

func (d *Detector) windowSize() int {
	if d.WindowSize > 0 {
		return d.WindowSize
	}
	return 100
}

// providerRateAndN returns the error rate and sample count for the
// given (model, provider) pair. Returns 0,0 when no samples yet.
func providerRateAndN(m map[providerKey]*ringBuf, pk providerKey) (float64, int) {
	buf, ok := m[pk]
	if !ok || buf.size == 0 {
		return 0, 0
	}
	vals := buf.values()
	s := 0.0
	for _, v := range vals {
		s += v
	}
	return s / float64(len(vals)), len(vals)
}

// humanInt formats a non-negative integer with thousands separators
// for human-friendly messages. Negative numbers are formatted as-is.
func humanInt(n int64) string {
	if n < 0 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	// Insert commas from the right.
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// shortHash returns the first 8 hex chars of a sha256 digest. Used
// for compact message rendering of prompt hashes.
func shortHash(h string) string {
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}