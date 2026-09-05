# Performance baseline

Snapshot of benchmark numbers from this repo at v0.1.0. Use to spot
regressions when running `go test -bench=. ./internal/bench/...`.

## Hardware

- Apple M4, darwin/arm64
- Go 1.23

## How to reproduce

```bash
go test -bench=. -benchmem -count=3 -run=^$ ./internal/bench/...
```

## Results (median of3, ns/op)

| Benchmark                       | ns/op    | B/op     | allocs/op |
|--------------------------------|----------|----------|-----------|
| BenchmarkSSEParseThroughput    |  64,773  | 169,569  |     2,002 |
| BenchmarkRedactorApply         |   1,567  |   1,621  |        21 |
| BenchmarkRingBufferAppend      |     34.7 |       0  |         0 |
| BenchmarkMetricsRecorder       |    203.2 |     870  |         0 |
| BenchmarkMetricsSummarize      |  21,364  | 155,705  |         4 |
| BenchmarkTokenEstimate         |   2,724  |       0  |         0 |

## What each number means

- **SSEParseThroughput** — parsing 1,000 SSE events (~120 KB). 64µs per
  1000 events = **~15.6 million events/sec** on this hardware. Way above
  any realistic LLM streaming rate (GPT-4o maxes around 200 tokens/sec).
- **RedactorApply** — 1.5µs per redact operation on a typical Bearer+api-key
  string. Negligible cost vs. the upstream HTTP round-trip it precedes.
- **RingBufferAppend** — 34.7ns per Append. Zero allocations because the
  ring pre-allocates its storage in `New()`. The hot loop in the proxy.
- **MetricsRecorder** — ~200ns per record insertion. The Recorder uses a
  mutex; on contention-heavy paths this can be a bottleneck. Watch for
  regressions here.
- **MetricsSummarize** — 21µs over 1,000 records. Acceptable for a
  1Hz refresh cycle but could matter if you crank the TUI to 60Hz.
- **TokenEstimate** — 2.7µs over a 12KB string. Linear in string length.

## Regression thresholds

If any benchmark regresses by **>25%** vs. this baseline, treat it as a
bug worth investigating before tagging a new release. The redactor and
metrics paths are the most likely to drift; the SSE parser and ring
buffer are simple enough that regressions usually mean something is
wrong (regex backtracking, accidental allocation, etc.).

## Adding a new benchmark

When you add a benchmark, also update this file. Run it on a clean
machine (no other workloads), take the median of 3 runs, and include
hardware info if it changed.