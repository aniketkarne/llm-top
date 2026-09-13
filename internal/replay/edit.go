package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aniketkarne-com/llm-top/internal/proxy"
)

// EditResult holds the outcome of an interactive edit session — the
// file path the user edited, the JSON they saved, and any error
// encountered (so callers can decide exit code without re-checking).
type EditResult struct {
	Path    string
	Body    string
	Skipped bool // true when EDITOR wasn't set and no fallback existed
}

// ErrInvalidJSON is returned by Edit when the user saves a temp file
// whose contents are not parseable JSON. Callers should exit with a
// specific code (2 in the CLI) to distinguish "broken JSON" from
// "transport failure".
var ErrInvalidJSON = errors.New("edit: saved file is not valid JSON")

// Edit opens the captured request body in $EDITOR (falling back to
// vi, then nano, then a sensible no-op), waits for the user to save,
// and returns the new body. The body is validated as JSON before
// returning; on failure the temp file path is preserved so the user
// can recover their edits.
//
// The temp file is created with a .json extension and 0600 perms.
// It is NOT cleaned up automatically — when validation fails the user
// might want to inspect it. The CLI subcommand can decide whether to
// remove it.
//
// Set EDITOR to a non-interactive command (e.g. "sed -i 's/x/y/'") to
// drive edits from tests without launching an actual editor.
func Edit(captured proxy.Request) (EditResult, error) {
	// Pretty-print the captured body so the editor opens something
	// readable. If the body is empty, seed with an empty object so
	// the editor still has valid JSON to start from.
	seed := captured.RequestBody
	if strings.TrimSpace(seed) == "" {
		seed = "{}"
	}
	var pretty interface{}
	if err := json.Unmarshal([]byte(seed), &pretty); err != nil {
		// Already-invalid JSON: surface it but still hand the user
		// the raw bytes so they can fix it themselves.
		return EditResult{}, fmt.Errorf("captured body is not valid JSON: %w", err)
	}
	pb, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		return EditResult{}, fmt.Errorf("pretty-print: %w", err)
	}

	f, err := os.CreateTemp("", "llm-top-edit-*.json")
	if err != nil {
		return EditResult{}, fmt.Errorf("temp file: %w", err)
	}
	path := f.Name()
	if _, err := f.Write(pb); err != nil {
		_ = f.Close()
		return EditResult{}, fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return EditResult{}, fmt.Errorf("close temp file: %w", err)
	}

	editor := pickEditor()
	if editor == "" {
		// No editor available at all — return the original body
		// untouched. The CLI will then pass the original through
		// to replay, which is a reasonable fallback.
		return EditResult{Path: path, Body: string(pb), Skipped: true},
			errors.New("no $EDITOR and no fallback (vi/nano) found; using original body")
	}

	cmd := exec.Command("sh", "-c", editor+" "+shellQuote(path))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return EditResult{Path: path}, fmt.Errorf("editor exited: %w", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return EditResult{Path: path}, fmt.Errorf("re-read temp file: %w", err)
	}
	bodyStr := string(body)
	if !json.Valid(body) {
		return EditResult{Path: path, Body: bodyStr}, ErrInvalidJSON
	}
	return EditResult{Path: path, Body: bodyStr}, nil
}

// pickEditor returns the first non-empty value among $EDITOR, vi,
// and nano. Returns "" when none of them is available, in which case
// the caller can decide whether to no-op or error.
func pickEditor() string {
	if v := os.Getenv("EDITOR"); v != "" {
		return v
	}
	for _, cand := range []string{"vi", "nano"} {
		if p, err := exec.LookPath(cand); err == nil && p != "" {
			return cand
		}
	}
	return ""
}
