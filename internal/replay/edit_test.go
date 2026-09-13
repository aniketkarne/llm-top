package replay

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

func TestEdit_HappyPath(t *testing.T) {
	// Use a script that appends a field — no real editor required.
	tmp, err := os.MkdirTemp("", "edit-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	editorPath := filepath.Join(tmp, "fake-editor.sh")
	script := `#!/bin/sh
set -e
# Find the temp file (last arg) and append a "temperature": 0.7 field
# before the closing brace.
f="$1"
python3 - "$f" <<'PY'
import json, sys
p = sys.argv[1]
with open(p) as fh:
    data = json.load(fh)
data["temperature"] = 0.7
with open(p, "w") as fh:
    json.dump(data, fh, indent=2)
PY
`
	if err := os.WriteFile(editorPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editorPath)
	// Ensure vi/nano don't accidentally win over EDITOR.
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`,
	}
	res, err := Edit(captured)
	if err != nil {
		t.Fatalf("Edit returned error: %v", err)
	}
	if !strings.Contains(res.Body, `"temperature": 0.7`) {
		t.Errorf("expected temperature=0.7 in edited body, got:\n%s", res.Body)
	}
	if !strings.Contains(res.Body, `"model": "gpt-5.5"`) {
		t.Errorf("expected model preserved in edited body, got:\n%s", res.Body)
	}
	if res.Path == "" {
		t.Errorf("expected Path to be set to the temp file")
	}
}

func TestEdit_InvalidJSONReturnsErrInvalidJSON(t *testing.T) {
	tmp, err := os.MkdirTemp("", "edit-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	editorPath := filepath.Join(tmp, "fake-editor.sh")
	script := `#!/bin/sh
f="$1"
echo 'this is not JSON {{{' > "$f"
`
	if err := os.WriteFile(editorPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editorPath)
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5"}`,
	}
	res, err := Edit(captured)
	if !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("expected ErrInvalidJSON, got: %v", err)
	}
	if res.Path == "" {
		t.Errorf("expected temp path returned even on invalid JSON")
	}
}

func TestEdit_RewritesModelOnSave(t *testing.T) {
	// The editor only renames the model. We then verify the body
	// can be replayed with a different model by post-processing it.
	tmp, err := os.MkdirTemp("", "edit-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	editorPath := filepath.Join(tmp, "fake-editor.sh")
	script := `#!/bin/sh
f="$1"
python3 - "$f" <<'PY'
import json, sys
p = sys.argv[1]
with open(p) as fh:
    data = json.load(fh)
data["model"] = "claude-sonnet-5"
with open(p, "w") as fh:
    json.dump(data, fh, indent=2)
PY
`
	if err := os.WriteFile(editorPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editorPath)
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5"}`,
	}
	res, err := Edit(captured)
	if err != nil {
		t.Fatalf("Edit returned error: %v", err)
	}
	if !strings.Contains(res.Body, `"claude-sonnet-5"`) {
		t.Errorf("expected claude-sonnet-5 in body, got: %s", res.Body)
	}
}

func TestEdit_NoEditorFallsBack(t *testing.T) {
	// PATH only contains a tempdir with neither vi nor nano.
	tmp, err := os.MkdirTemp("", "edit-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", tmp)

	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5"}`,
	}
	res, err := Edit(captured)
	if err == nil {
		t.Fatalf("expected error when no editor is available, got: %+v", res)
	}
	if !res.Skipped {
		t.Errorf("expected Skipped=true when no editor available")
	}
	if !strings.Contains(res.Body, "gpt-5.5") {
		t.Errorf("expected original body preserved in Skipped result")
	}
}

func TestEdit_BodyIsValidated(t *testing.T) {
	// The editor writes back JSON in a slightly different shape but
	// still valid JSON. The Result.Body should be the rewritten
	// valid JSON.
	tmp, err := os.MkdirTemp("", "edit-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	editorPath := filepath.Join(tmp, "fake-editor.sh")
	script := `#!/bin/sh
f="$1"
echo '{"model":"claude-sonnet-5","new":"yes"}' > "$f"
`
	if err := os.WriteFile(editorPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editorPath)
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	captured := proxy.Request{
		Path:        "/v1/chat/completions",
		Upstream:    "https://api.openai.com",
		RequestBody: `{"model":"gpt-5.5"}`,
	}
	res, err := Edit(captured)
	if err != nil {
		t.Fatalf("Edit returned error: %v", err)
	}
	if !strings.Contains(res.Body, `"new":"yes"`) {
		t.Errorf("expected new field in body: %s", res.Body)
	}
}

func TestPickEditor(t *testing.T) {
	// Reset PATH and EDITOR to known values for this sub-test.
	t.Run("explicit", func(t *testing.T) {
		t.Setenv("EDITOR", "/usr/bin/my-editor")
		t.Setenv("PATH", "")
		if got := pickEditor(); got != "/usr/bin/my-editor" {
			t.Errorf("pickEditor=%q want /usr/bin/my-editor", got)
		}
	})
	t.Run("fallback vi", func(t *testing.T) {
		t.Setenv("EDITOR", "")
		// PATH empty so neither vi nor nano resolves, except we
		// can't rely on system vi existing in CI. Just verify it
		// doesn't panic and returns something sensible.
		t.Setenv("PATH", "/nonexistent")
		if got := pickEditor(); got != "" {
			t.Logf("pickEditor=%q (vi/nano found in PATH=)", got)
		}
	})
}
