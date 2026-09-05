// Command llm-top is a single-binary OpenAI-compatible reverse proxy with a
// built-in terminal dashboard, SSE interception, per-request metrics, and a
// fixed-size circular buffer for prompt and response capture.
//
// Usage:
//
//	llm-top proxy   --listen 127.0.0.1:7777 --upstream https://api.openai.com
//	llm-top ui      --listen 127.0.0.1:7777
//	llm-top                 # integrated mode (proxy + UI)
//
// All flags have corresponding LLMTOP_* environment variables and a JSON
// config file. See the README for details.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/config"
	"github.com/aniketkarne-com/llm-top/internal/demo"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
	"github.com/aniketkarne-com/llm-top/internal/proxy"
	"github.com/aniketkarne-com/llm-top/internal/redactor"
	"github.com/aniketkarne-com/llm-top/internal/store"
	"github.com/aniketkarne-com/llm-top/internal/ui"
)

// version is set via -ldflags at build time.
var version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "llm-top:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Default to "integrated" mode when no subcommand is given. If the user
	// passes an explicit mode flag, we honor that. If they pass a known
	// subcommand, we strip it before delegating to config.Load.
	mode := "integrated"
	filtered := args
	if len(args) > 0 {
		switch args[0] {
		case "proxy", "ui", "integrated":
			mode = args[0]
			filtered = args[1:]
		case "version", "-version", "--version":
			fmt.Println("llm-top", version)
			return nil
		case "help", "-help", "--help", "-h":
			printUsage(os.Stdout)
			return nil
		case "demo":
			return runDemo()
		}
	}

	cfg, err := config.Load(append([]string{"--mode", mode}, filtered...), "")
	if err != nil {
		return err
	}

	if cfg.UpstreamAPIKey == "" {
		// Fall back to OPENAI_API_KEY if LLMTOP_API_KEY isn't set.
		if v := os.Getenv("OPENAI_API_KEY"); v != "" {
			cfg.UpstreamAPIKey = v
		}
	}

	// Validate that the proxy port is bindable before we commit to running.
	if cfg.Mode == "proxy" || cfg.Mode == "integrated" {
		if err := proxy.CheckAvailable(cfg.ListenAddr); err != nil {
			return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
		}
	}

	rec := metrics.NewRecorder()
	buf := ring.New(cfg.BufferSize)
	red := redactor.New()

	// Optional SQLite persistence. Failures to open the DB are non-fatal:
	// the proxy still runs, just without a session dump.
	var st *store.Store
	if cfg.SQLitePath != "" {
		abs, _ := filepath.Abs(cfg.SQLitePath)
		s, err := store.Open(abs)
		if err == nil {
			st = s
		} else {
			fmt.Fprintf(os.Stderr, "llm-top: sqlite unavailable (%v); continuing without persistence\n", err)
		}
	}

	// Hook the buffer to also persist events when SQLite is enabled.
	// We do this by wrapping the ring's append in a tee that fans out.
	// To avoid leaking the abstraction, we instead poll on a ticker and
	// snapshot anything new. This keeps ring package zero-dependency.
	if st != nil {
		go pumpToStore(st, buf)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var srv *proxy.Server
	var statsProv ui.StatsProvider = nullStats{}

	if cfg.Mode == "proxy" || cfg.Mode == "integrated" {
		srv = proxy.New(proxy.Config{
			ListenAddr:      cfg.ListenAddr,
			UpstreamBaseURL: cfg.UpstreamBaseURL,
			UpstreamAPIKey:  cfg.UpstreamAPIKey,
			BufferSize:      cfg.BufferSize,
		}, rec, buf, red)
		statsProv = srv
		go func() {
			if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, "llm-top proxy exited:", err)
			}
		}()
	}

	if cfg.Mode == "ui" || cfg.Mode == "integrated" {
		// If we didn't start a proxy server, the stats provider is a stub.
		if statsProv == nil {
			statsProv = nullStats{}
		}
		model := ui.New(rec, buf, statsProv, cfg.ListenAddr, cfg.UpstreamBaseURL)
		isTTY := isTerminal(os.Stdout)
		go func() {
			err := model.Run(os.Stdout, 500*time.Millisecond, isTTY)
			if err != nil {
				fmt.Fprintln(os.Stderr, "llm-top ui exited:", err)
			}
			cancel()
		}()
	}

	// Wait for signal.
	<-ctx.Done()

	// Graceful shutdown of SQLite.
	if st != nil {
		_ = st.Close()
	}
	return nil
}

// nullStats satisfies ui.StatsProvider when no proxy is running.
type nullStats struct{}

func (nullStats) Stats() (uint64, uint64) { return 0, 0 }

// pumpToStore copies newly-appended ring entries into the SQLite store. It
// runs on a 250ms ticker and only persists entries it hasn't seen yet by
// remembering the last id observed.
func pumpToStore(st *store.Store, buf *ring.Buffer) {
	seen := 0
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		snap := buf.Snapshot()
		if len(snap) <= seen {
			continue
		}
		for _, e := range snap[seen:] {
			_ = st.Append(store.Event{Time: e.Time, Kind: e.Kind, Model: e.Source, Content: e.Content})
		}
		seen = len(snap)
	}
}

// isTerminal returns true if f is connected to a TTY. We avoid pulling in
// golang.org/x/term by checking the common env hints that macOS/Linux shells
// always export on real terminals.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	if (st.Mode() & os.ModeCharDevice) == 0 {
		return false
	}
	// Additional hint: NO_COLOR or CI indicates non-interactive.
	if os.Getenv("NO_COLOR") != "" || os.Getenv("CI") != "" {
		return false
	}
	return true
}

// runDemo executes the zero-deps showcase. See internal/demo for details.
// Defaults are tuned to finish in ~300ms so this works as a one-line
// first-run experience.
func runDemo() error {
	res, err := demo.Run(demo.Options{})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"\nllm-top demo complete: %d requests, %d output tokens, %d ring entries, %s total\n",
		res.Requests, res.OutputTokens, res.BufferEntries, res.Duration.Round(time.Millisecond),
	)
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, strings.TrimSpace(`
llm-top — OpenAI-compatible HTTP proxy with TUI dashboard, SSE metrics, and prompt capture.

Usage:
  llm-top [subcommand] [flags]

Subcommands:
  proxy        run only the HTTP proxy (no TUI)
  ui           run only the TUI (connects to an existing proxy)
  integrated   run proxy + TUI together (default)
  demo         zero-deps end-to-end showcase (no API key, no network)
  version      print version and exit
  help         print this message

Flags:
  --listen       address to listen on               (default 127.0.0.1:7777)
  --upstream     upstream base URL                  (default https://api.openai.com)
  --api-key      upstream API key                   (or LLMTOP_API_KEY / OPENAI_API_KEY)
  --buffer       circular buffer capacity           (default 500, max 100000)
  --ui           launch TUI alongside the proxy     (default true in integrated mode)
  --sqlite       optional SQLite session-dump path  (e.g. sessions.db)
  --mode         proxy | ui | integrated            (default integrated)

Environment variables (override defaults; flags override env):
  LLMTOP_LISTEN, LLMTOP_UPSTREAM, LLMTOP_API_KEY, LLMTOP_BUFFER,
  LLMTOP_UI, LLMTOP_SQLITE, LLMTOP_MODE
`))
}
