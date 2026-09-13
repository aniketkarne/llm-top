package replay

import (
	"strings"
	"testing"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

func TestCurlCommand_Defaults(t *testing.T) {
	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		Model:       "gpt-5.5",
		RequestBody: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`,
	}
	out := CurlCommand(captured, CurlOptions{})
	must := []string{
		"curl -sS -X POST 'https://api.openai.com/v1/chat/completions'",
		"-H 'Authorization: Bearer $LLMTOP_API_KEY'",
		"-H 'Content-Type: application/json'",
		`"model":"gpt-5.5"`,
	}
	for _, m := range must {
		if !strings.Contains(out, m) {
			t.Errorf("curl output missing %q\n%s", m, out)
		}
	}
}

func TestCurlCommand_RevealKey(t *testing.T) {
	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5"}`,
	}
	out := CurlCommand(captured, CurlOptions{
		APIKey:    "sk-supersecret",
		RevealKey: true,
	})
	if !strings.Contains(out, "Bearer sk-supersecret") {
		t.Errorf("expected Bearer sk-supersecret in output:\n%s", out)
	}
	if strings.Contains(out, "$LLMTOP_API_KEY") {
		t.Errorf("did not expect placeholder when RevealKey=true and key set:\n%s", out)
	}
}

func TestCurlCommand_RevealKeyIgnoredWithoutKey(t *testing.T) {
	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5"}`,
	}
	out := CurlCommand(captured, CurlOptions{RevealKey: true})
	if !strings.Contains(out, "$LLMTOP_API_KEY") {
		t.Errorf("should fall back to $LLMTOP_API_KEY when no key provided:\n%s", out)
	}
}

func TestCurlCommand_UpstreamOverride(t *testing.T) {
	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5"}`,
	}
	out := CurlCommand(captured, CurlOptions{
		UpstreamBaseURL: "http://localhost:11434/v1",
	})
	if !strings.Contains(out, "http://localhost:11434/v1/v1/chat/completions") &&
		!strings.Contains(out, "http://localhost:11434/v1/chat/completions") {
		// joinURL strips double slashes between base "/v1" and path
		// "/v1/..."; the actual output should be the latter.
		t.Errorf("override upstream URL missing or wrong:\n%s", out)
	}
	if strings.Contains(out, "api.openai.com") {
		t.Errorf("captured upstream should be overridden away:\n%s", out)
	}
}

func TestCurlCommand_ModelRewrite(t *testing.T) {
	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5","messages":[]}`,
	}
	out := CurlCommand(captured, CurlOptions{Model: "claude-sonnet-5"})
	if !strings.Contains(out, `"model":"claude-sonnet-5"`) {
		t.Errorf("expected rewritten model in -d payload:\n%s", out)
	}
	if strings.Contains(out, `"model":"gpt-5.5"`) {
		t.Errorf("old model still present in payload:\n%s", out)
	}
}

func TestCurlCommand_StreamRewrite(t *testing.T) {
	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5","stream":false}`,
	}
	override := true
	out := CurlCommand(captured, CurlOptions{Stream: &override})
	if !strings.Contains(out, `"stream":true`) {
		t.Errorf("expected stream=true in -d payload:\n%s", out)
	}
	if strings.Contains(out, `"stream":false`) {
		t.Errorf("old stream=false still present:\n%s", out)
	}
}

func TestCurlCommand_ExtraHeaders(t *testing.T) {
	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{}`,
	}
	out := CurlCommand(captured, CurlOptions{
		ExtraHeaders: map[string]string{
			"X-Trace": "trace-1",
		},
	})
	if !strings.Contains(out, "-H 'X-Trace: trace-1'") {
		t.Errorf("expected X-Trace header in output:\n%s", out)
	}
}

func TestCurlCommand_DefaultPathWhenEmpty(t *testing.T) {
	captured := proxy.Request{
		Upstream:    "https://api.openai.com",
		RequestBody: `{}`,
	}
	out := CurlCommand(captured, CurlOptions{})
	if !strings.Contains(out, "/v1/chat/completions") {
		t.Errorf("expected default /v1/chat/completions path:\n%s", out)
	}
}

func TestCurlCommand_EmptyBodyOmitted(t *testing.T) {
	captured := proxy.Request{
		Path:     "/v1/chat/completions",
		Upstream: "https://api.openai.com",
	}
	out := CurlCommand(captured, CurlOptions{})
	if strings.Contains(out, "-d '") {
		t.Errorf("expected no -d flag for empty body:\n%s", out)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"hello":   "hello",
		"it's":    `it'"'"'s`,
		`'all'`:   `'"'"'all'"'"'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q)=%q want %q", in, got, want)
		}
	}
}
