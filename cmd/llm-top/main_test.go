package main

import (
	"bytes"
	"strings"
	"testing"
)

// --- CLI subcommand smoke tests ---

func TestVersionSubcommand(t *testing.T) {
	for _, flag := range []string{"version", "-version", "--version"} {
		if err := run([]string{flag}); err != nil {
			t.Errorf("run %s: %v", flag, err)
		}
	}
}

func TestHelpSubcommand(t *testing.T) {
	for _, flag := range []string{"help", "-help", "--help", "-h"} {
		if err := run([]string{flag}); err != nil {
			t.Errorf("run %s: %v", flag, err)
		}
	}
}

func TestPrintUsageContainsSubcommands(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	out := buf.String()
	for _, sub := range []string{"proxy", "ui", "integrated", "version", "help", "demo"} {
		if !strings.Contains(out, sub) {
			t.Errorf("usage text missing subcommand %q", sub)
		}
	}
}

func TestPrintUsageContainsFlags(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	out := buf.String()
	for _, flag := range []string{"--listen", "--upstream", "--api-key", "--buffer", "--sqlite", "--mode"} {
		if !strings.Contains(out, flag) {
			t.Errorf("usage text missing flag %q", flag)
		}
	}
}

func TestPrintUsageContainsEnvVars(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	out := buf.String()
	for _, env := range []string{"LLMTOP_LISTEN", "LLMTOP_UPSTREAM", "LLMTOP_API_KEY"} {
		if !strings.Contains(out, env) {
			t.Errorf("usage text missing env var %q", env)
		}
	}
}

func TestRunRejectsBadMode(t *testing.T) {
	// Unknown mode should not crash; config.Load will return an error or
	// accept the value depending on impl. We just verify no panic.
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("run with unknown mode panicked: %v", rec)
		}
	}()
	_ = run([]string{"--mode", "definitely-not-a-real-mode"})
}