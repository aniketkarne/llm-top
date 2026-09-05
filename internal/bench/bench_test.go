package bench

// This package exists as a holder for benchmark entry points and baseline
// numbers. Run with:
//
//   go test -bench=. ./internal/bench/...
//
// Results are intended to be compared against BASELINE.md in this directory.

import (
	"strings"
	"testing"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
	"github.com/aniketkarne-com/llm-top/internal/redactor"
	"github.com/aniketkarne-com/llm-top/internal/sse"
)

// BenchmarkSSEParseThroughput exercises the SSE parser against a realistic
// chunk stream.
func BenchmarkSSEParseThroughput(b *testing.B) {
	// Build a stream with 1000 events, each ~80 bytes.
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString(`data: {"choices":[{"delta":{"content":"hello world this is token `)
		sb.WriteString(strings.Repeat("a", 10))
		sb.WriteString(`"}}]}` + "\n\n")
	}
	input := sb.String()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := sse.New(strings.NewReader(input))
		for {
			_, err := p.NextEvent()
			if err != nil {
				break
			}
		}
	}
}

// BenchmarkRedactorApply measures how fast the redactor scrubs secrets.
func BenchmarkRedactorApply(b *testing.B) {
	r := redactor.New()
	in := `Authorization: Bearer eyJhbGciOi.payload.sig api_key=sk-abcdef1234567890abcdef password=hunter2 normal text normal text normal text normal text`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Apply(in)
	}
}

// BenchmarkRingBufferAppend measures raw buffer append throughput.
func BenchmarkRingBufferAppend(b *testing.B) {
	buf := ring.New(10000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Append("k", "src", "hello world this is content")
	}
}

// BenchmarkMetricsRecorder measures the cost of adding a metric record.
func BenchmarkMetricsRecorder(b *testing.B) {
	rec := metrics.NewRecorder()
	rec.Add(metrics.Record{Total: 100_000_000, OutputTok: 100})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Add(metrics.Record{Total: 100_000_000, OutputTok: 100})
	}
}

// BenchmarkMetricsSummarize measures summary aggregation cost on a large
// recorder.
func BenchmarkMetricsSummarize(b *testing.B) {
	rec := metrics.NewRecorder()
	for i := 0; i < 1000; i++ {
		rec.Add(metrics.Record{Total: 100_000_000, OutputTok: 100, Status: 200})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = rec.Summarize()
	}
}

// BenchmarkTokenEstimate measures token-counting on a long string.
func BenchmarkTokenEstimate(b *testing.B) {
	s := strings.Repeat("hello world ", 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = metrics.EstimateTokens(s)
	}
}