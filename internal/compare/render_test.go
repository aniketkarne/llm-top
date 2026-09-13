package compare

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleRows() []Row {
	return []Row{
		{
			Name: "gpt-5.5", Model: "gpt-5.5", Provider: "openai",
			BaseURL: "https://api.openai.com", StatusCode: 200,
			Stream: false, TTFTMillis: 420, TotalMillis: 2810,
			PromptTokens: 8421, OutputTokens: 734, CostUSD: 0.08, CostKnown: true,
			ResponseBody: "The capital of France is Paris.",
		},
		{
			Name: "claude-sonnet-5", Model: "claude-sonnet-5", Provider: "anthropic",
			BaseURL: "https://api.anthropic.com", StatusCode: 200,
			Stream: false, TTFTMillis: 610, TotalMillis: 3220,
			PromptTokens: 8421, OutputTokens: 691, CostUSD: 0.05, CostKnown: true,
			ResponseBody: "Paris is the capital of France.",
		},
		{
			Name: "local", Model: "llama-3.1-8b", Provider: "local",
			BaseURL: "http://localhost:11434/v1", StatusCode: 200,
			Stream: false, TTFTMillis: 91, TotalMillis: 1940,
			PromptTokens: 8421, OutputTokens: 812, CostUSD: 0, CostKnown: false,
			ResponseBody: "Paris.",
		},
	}
}

func TestRenderTextHappy(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, "abc123", sampleRows())
	out := buf.String()

	// Header anchor.
	if !strings.Contains(out, "compare: captured=abc123") {
		t.Errorf("missing captured-id header: %s", out)
	}
	// Column headers.
	for _, want := range []string{"gpt-5.5", "claude-sonnet-5", "local"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing column header %q in:\n%s", want, out)
		}
	}
	// Status row.
	if !strings.Contains(out, "status") {
		t.Errorf("missing status row")
	}
	// TTFT values from the sketch (only gpt and claude have non-zero TTFT).
	if !strings.Contains(out, "420ms") || !strings.Contains(out, "610ms") || !strings.Contains(out, "91ms") {
		t.Errorf("missing TTFT values: %s", out)
	}
	// Total rendered as seconds for >=1000ms.
	if !strings.Contains(out, "2.81s") || !strings.Contains(out, "3.22s") || !strings.Contains(out, "1.94s") {
		t.Errorf("missing total-as-seconds: %s", out)
	}
	// Tokens.
	if !strings.Contains(out, "8,421") {
		t.Errorf("missing prompt-token thousand-separator")
	}
	if !strings.Contains(out, "734") || !strings.Contains(out, "691") || !strings.Contains(out, "812") {
		t.Errorf("missing output tokens: %s", out)
	}
	// Cost. Two decimal places for known, $? for unknown.
	if !strings.Contains(out, "$0.08") || !strings.Contains(out, "$0.05") {
		t.Errorf("missing known costs")
	}
	if !strings.Contains(out, "$?") {
		t.Errorf("missing $? for unknown-cost row")
	}
	// Excerpts section present.
	if !strings.Contains(out, "excerpts:") {
		t.Errorf("missing excerpts section")
	}
	if !strings.Contains(out, "The capital of France is Paris.") {
		t.Errorf("missing excerpt text")
	}
}

func TestRenderTextNoTargets(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, "id", nil)
	if !strings.Contains(buf.String(), "(no targets)") {
		t.Errorf("expected empty-target message, got %q", buf.String())
	}
}

func TestRenderTextAllError(t *testing.T) {
	rows := []Row{
		{Name: "a", StatusCode: 0, Error: "upstream unreachable"},
		{Name: "b", StatusCode: 0, Error: "context deadline exceeded"},
	}
	var buf bytes.Buffer
	RenderText(&buf, "id", rows)
	out := buf.String()
	if !strings.Contains(out, "ERR") {
		t.Errorf("missing ERR status marker: %s", out)
	}
	if !strings.Contains(out, "errors:") {
		t.Errorf("missing errors section: %s", out)
	}
	if !strings.Contains(out, "upstream unreachable") {
		t.Errorf("error text missing: %s", out)
	}
	// No excerpts when no body.
	if strings.Contains(out, "excerpts:") {
		t.Errorf("excerpts should be absent when all failed: %s", out)
	}
}

func TestRenderTextPartialFailure(t *testing.T) {
	rows := []Row{
		{Name: "ok", StatusCode: 200, Model: "gpt-5.5-mini", TotalMillis: 1234, OutputTokens: 5, CostUSD: 0.08, CostKnown: true, ResponseBody: "hello"},
		{Name: "down", StatusCode: 0, Error: "connection refused"},
	}
	var buf bytes.Buffer
	RenderText(&buf, "id", rows)
	out := buf.String()
	// Status values are present (tabwriter may consume the literal 	
	// we wrote between label and value, replacing it with padding).
	if !strings.Contains(out, "200") {
		t.Errorf("ok status 200 missing: %s", out)
	}
	if !strings.Contains(out, "ERR") {
		t.Errorf("down ERR status missing: %s", out)
	}
	// Excerpts only for the ok row. Look only inside the excerpts section,
	// not the errors section that follows (which has its own "down:" line).
	excerptsIdx := strings.Index(out, "excerpts:")
	errorsIdx := strings.Index(out, "errors:")
	if excerptsIdx < 0 || (errorsIdx > 0 && errorsIdx < excerptsIdx) {
		t.Fatalf("section ordering wrong: excerptsIdx=%d errorsIdx=%d", excerptsIdx, errorsIdx)
	}
	between := out[excerptsIdx:errorsIdx]
	if !strings.Contains(between, "ok: hello") {
		t.Errorf("ok excerpt missing in excerpts section: %s", between)
	}
	if strings.Contains(between, "down") {
		t.Errorf("down excerpt should be absent from excerpts section: %s", between)
	}
}

func TestRenderTextStreamingTTFT(t *testing.T) {
	rows := []Row{
		{Name: "stream", Stream: true, StatusCode: 200, Model: "claude-sonnet-5", TTFTMillis: 250, TotalMillis: 1800, OutputTokens: 50, CostUSD: 0.001, CostKnown: true, ResponseBody: "ok"},
	}
	var buf bytes.Buffer
	RenderText(&buf, "id", rows)
	out := buf.String()
	if !strings.Contains(out, "250ms") {
		t.Errorf("streaming TTFT not rendered: %s", out)
	}
}

func TestRenderTextSubSecondTotal(t *testing.T) {
	rows := []Row{
		{Name: "fast", StatusCode: 200, Model: "gpt-5.5-mini", TotalMillis: 420, OutputTokens: 1, CostUSD: 0.0001, CostKnown: true},
	}
	var buf bytes.Buffer
	RenderText(&buf, "id", rows)
	out := buf.String()
	if !strings.Contains(out, "420ms") {
		t.Errorf("sub-second total should render in ms, not seconds: %s", out)
	}
	// And specifically NOT as "0.42s".
	if strings.Contains(out, "0.42s") {
		t.Errorf("420ms should not be promoted to seconds: %s", out)
	}
}

func TestRenderTextExcerptTruncation(t *testing.T) {
	longBody := strings.Repeat("abcdefghij", MaxExcerptBytes/5) // > MaxExcerptBytes
	rows := []Row{
		{Name: "x", StatusCode: 200, Model: "m", TotalMillis: 100, OutputTokens: 1, CostUSD: 0.001, CostKnown: true, ResponseBody: longBody},
	}
	var buf bytes.Buffer
	RenderText(&buf, "id", rows)
	out := buf.String()
	// The excerpt line should end with "…".
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  x: ") {
			if !strings.HasSuffix(line, "…") {
				t.Errorf("excerpt line should end with ellipsis: %q", line)
			}
			// The prefix is "  x: " (5 chars); the line should be at most
			// 5 + MaxExcerptBytes + UTF-8 bytes of "…".
			if len(line) > 5+MaxExcerptBytes+10 {
				t.Errorf("excerpt line too long: %d", len(line))
			}
		}
	}
}

func TestFormatNumber(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{812, "812"},
		{8421, "8,421"},
		{1000000, "1,000,000"},
		{-12345, "-12,345"},
	}
	for _, c := range cases {
		if got := formatNumber(c.in); got != c.want {
			t.Errorf("formatNumber(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderJSON(t *testing.T) {
	var buf bytes.Buffer
	res := CompareResult{CapturedID: "abc", Rows: sampleRows()}
	if err := RenderJSON(&buf, res); err != nil {
		t.Fatal(err)
	}
	var got CompareResult
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("JSON did not round-trip: %v\n%s", err, buf.String())
	}
	if got.CapturedID != "abc" || len(got.Rows) != 3 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Rows[0].Name != "gpt-5.5" || got.Rows[2].CostKnown {
		t.Errorf("field mismatch: %+v", got.Rows)
	}
}
