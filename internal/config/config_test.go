package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("default validate: %v", err)
	}
}

func TestValidateRejectsBadURL(t *testing.T) {
	c := Default()
	c.UpstreamBaseURL = "ftp://x"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for bad scheme")
	}
}

func TestValidateRejectsBadMode(t *testing.T) {
	c := Default()
	c.Mode = "nope"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for bad mode")
	}
}

func TestLoadFromJSONFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(p, []byte(`{"listen_addr":"127.0.0.1:9000","buffer_size":42,"ui":false,"mode":"proxy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(nil, p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ListenAddr != "127.0.0.1:9000" || c.BufferSize != 42 || c.UI || c.Mode != "proxy" {
		t.Fatalf("loaded wrong: %+v", c)
	}
}

func TestEnvOverridesDefaults(t *testing.T) {
	t.Setenv("LLMTOP_LISTEN", "127.0.0.1:1234")
	t.Setenv("LLMTOP_BUFFER", "10")
	t.Setenv("LLMTOP_UI", "false")
	t.Setenv("LLMTOP_MODE", "ui")
	c, err := Load(nil, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ListenAddr != "127.0.0.1:1234" || c.BufferSize != 10 || c.UI || c.Mode != "ui" {
		t.Fatalf("env not applied: %+v", c)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	t.Setenv("LLMTOP_LISTEN", "127.0.0.1:1234")
	c, err := Load([]string{"-listen", "127.0.0.1:5555"}, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ListenAddr != "127.0.0.1:5555" {
		t.Fatalf("flag override failed: %s", c.ListenAddr)
	}
}

func TestYAMLSubset(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(p, []byte("listen_addr: 127.0.0.1:7778\nbuffer_size: 12\nui: true\nmode: integrated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(nil, p)
	if err != nil {
		t.Fatalf("load yaml: %v", err)
	}
	if c.BufferSize != 12 || c.Mode != "integrated" {
		t.Fatalf("yaml load wrong: %+v", c)
	}
}
