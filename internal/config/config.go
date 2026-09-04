// Package config loads and validates llm-top runtime configuration.
//
// Configuration sources (highest priority first):
//  1. CLI flags
//  2. Environment variables (LLMTOP_*)
//  3. Optional config file (JSON or YAML, detected by extension)
//
// Example JSON:
//
//	{
//	  "listen_addr": "127.0.0.1:7777",
//	  "upstream_base_url": "https://api.openai.com",
//	  "upstream_api_key": "sk-...",
//	  "buffer_size": 500,
//	  "ui": true,
//	  "sqlite_path": "sessions.db"
//	}
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the parsed llm-top configuration.
type Config struct {
	ListenAddr      string `json:"listen_addr"`
	UpstreamBaseURL string `json:"upstream_base_url"`
	UpstreamAPIKey  string `json:"upstream_api_key"`
	BufferSize      int    `json:"buffer_size"`
	UI              bool   `json:"ui"`
	SQLitePath      string `json:"sqlite_path"`
	Mode            string `json:"mode"` // "proxy", "ui", or "integrated"
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{
		ListenAddr:      "127.0.0.1:7777",
		UpstreamBaseURL: "https://api.openai.com",
		BufferSize:      500,
		UI:              true,
		Mode:            "integrated",
	}
}

// Validate ensures required fields are present and within bounds.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	if c.UpstreamBaseURL == "" {
		return errors.New("upstream_base_url is required")
	}
	if !strings.HasPrefix(c.UpstreamBaseURL, "http://") && !strings.HasPrefix(c.UpstreamBaseURL, "https://") {
		return fmt.Errorf("upstream_base_url must be http:// or https://, got %q", c.UpstreamBaseURL)
	}
	if c.BufferSize <= 0 || c.BufferSize > 100000 {
		return fmt.Errorf("buffer_size out of range: %d", c.BufferSize)
	}
	switch c.Mode {
	case "proxy", "ui", "integrated":
	default:
		return fmt.Errorf("mode must be one of proxy|ui|integrated, got %q", c.Mode)
	}
	return nil
}

// Load resolves configuration from file, env, and CLI flags.
// args should be the post-binary CLI args (excluding the binary name).
// filePath may be "" to skip file-based config.
func Load(args []string, filePath string) (Config, error) {
	cfg := Default()

	if filePath != "" {
		if err := loadFile(&cfg, filePath); err != nil {
			return cfg, fmt.Errorf("config file: %w", err)
		}
	}

	applyEnv(&cfg)

	fs := flag.NewFlagSet("llm-top", flag.ContinueOnError)
	listen := fs.String("listen", cfg.ListenAddr, "address to listen on")
	upstream := fs.String("upstream", cfg.UpstreamBaseURL, "upstream base URL (OpenAI-compatible)")
	apiKey := fs.String("api-key", cfg.UpstreamAPIKey, "upstream API key (overrides config)")
	bufSize := fs.Int("buffer", cfg.BufferSize, "circular buffer capacity (max 500 by default)")
	ui := fs.Bool("ui", cfg.UI, "launch TUI alongside the proxy")
	sqlitePath := fs.String("sqlite", cfg.SQLitePath, "optional SQLite path for session dump")
	mode := fs.String("mode", cfg.Mode, "run mode: proxy | ui | integrated")
	configPath := fs.String("config", filePath, "path to config file (JSON)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	cfg.ListenAddr = *listen
	cfg.UpstreamBaseURL = *upstream
	cfg.UpstreamAPIKey = *apiKey
	cfg.BufferSize = *bufSize
	cfg.UI = *ui
	cfg.SQLitePath = *sqlitePath
	cfg.Mode = *mode

	// Allow --config to point at a file that should be loaded last (overrides env, etc).
	if cp := *configPath; cp != "" && cp != filePath {
		if err := loadFile(&cfg, cp); err != nil {
			return cfg, fmt.Errorf("config file: %w", err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func loadFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".yaml" || ext == ".yml" {
		// Minimal YAML support: we only accept a flat key:value subset that
		// round-trips through JSON, so we translate colon-delimited lines.
		// For richer YAML users can convert to JSON.
		translated, terr := flatYAMLToJSON(data)
		if terr != nil {
			return terr
		}
		data = translated
	}
	return json.Unmarshal(data, cfg)
}

// flatYAMLToJSON converts a very small subset of YAML — top-level
// "key: value" lines with string/int/bool values — into JSON. It is
// deliberately minimal; users with complex YAML should use JSON.
func flatYAMLToJSON(in []byte) ([]byte, error) {
	out := map[string]any{}
	for _, line := range strings.Split(string(in), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		colon := strings.Index(trim, ":")
		if colon <= 0 {
			return nil, fmt.Errorf("unsupported YAML line: %q", line)
		}
		k := strings.TrimSpace(trim[:colon])
		v := strings.TrimSpace(trim[colon+1:])
		v = strings.Trim(v, `"'`)
		switch v {
		case "true":
			out[k] = true
		case "false":
			out[k] = false
		default:
			if n, err := parseInt(v); err == nil {
				out[k] = n
			} else {
				out[k] = v
			}
		}
	}
	return json.Marshal(out)
}

func parseInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not int")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("LLMTOP_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("LLMTOP_UPSTREAM"); v != "" {
		cfg.UpstreamBaseURL = v
	}
	if v := os.Getenv("LLMTOP_API_KEY"); v != "" {
		cfg.UpstreamAPIKey = v
	}
	if v := os.Getenv("LLMTOP_BUFFER"); v != "" {
		if n, err := parseInt(v); err == nil {
			cfg.BufferSize = n
		}
	}
	if v := os.Getenv("LLMTOP_UI"); v == "1" || strings.EqualFold(v, "true") {
		cfg.UI = true
	} else if v == "0" || strings.EqualFold(v, "false") {
		cfg.UI = false
	}
	if v := os.Getenv("LLMTOP_SQLITE"); v != "" {
		cfg.SQLitePath = v
	}
	if v := os.Getenv("LLMTOP_MODE"); v != "" {
		cfg.Mode = v
	}
}
