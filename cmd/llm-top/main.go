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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
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
		case "requests":
			return runRequests(filtered[1:])
		case "show":
			rest := filtered[1:]
			if len(rest) < 1 || strings.HasPrefix(rest[0], "-") {
				return fmt.Errorf("usage: llm-top show <request-id> [--sqlite <path>]")
			}
			return runShow(rest[0], strings.Join(rest[1:], " "))
		case "diff":
			rest := filtered[1:]
			if len(rest) < 2 || strings.HasPrefix(rest[0], "-") || strings.HasPrefix(rest[1], "-") {
				return fmt.Errorf("usage: llm-top diff <id-a> <id-b> [--sqlite <path>]")
			}
			return runDiff(rest[0], rest[1], strings.Join(rest[2:], " "))
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
		if st != nil {
			srv.WithStore(st)
		}
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

// sqliteDBPath returns the SQLite database path from --sqlite, or the default.
// Used by the read-only CLI subcommands (requests/show/diff) that need to
// open the DB without going through the full config loader.
func defaultSQLitePath() string {
	if v := os.Getenv("LLMTOP_SQLITE"); v != "" {
		return v
	}
	return "llm-top.db"
}

// openCLIStore opens the store at the path given by --sqlite (or default)
// and registers cleanup so the file handle is released.
func openCLIStore(flag string) (*store.Store, func(), error) {
	path := flag
	if path == "" {
		path = defaultSQLitePath()
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil, fmt.Errorf("no sqlite db at %s (set --sqlite <path> or LLMTOP_SQLITE; run llm-top with --sqlite to create one)", path)
	}
	st, err := store.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return st, func() { _ = st.Close() }, nil
}

// runRequests lists recent captured requests. Flags:
//
//	--limit N    maximum rows (default 50)
//	--model X    filter by model
//	--since DUR  show only requests newer than DUR (e.g. 1h, 30m, 2h30m)
//	--json       emit JSON instead of a table
func runRequests(args []string) error {
	fs := flag.NewFlagSet("requests", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "max rows to show")
	model := fs.String("model", "", "filter by model name")
	since := fs.Duration("since", 0, "only show requests newer than this (e.g. 1h, 30m)")
	asJSON := fs.Bool("json", false, "emit JSON")
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()

	filter := store.ListFilter{
		Model: *model,
		Limit: *limit,
	}
	if *since > 0 {
		filter.Since = time.Now().Add(-*since).UTC()
	}
	rows, err := st.List(filter)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no requests matched the filter")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTARTED\tMODEL\tPROVIDER\tSTATUS\tTTFT\tTOTAL\tIN\tOUT\tCOST\tERR")
	for _, r := range rows {
		ttft := "-"
		if r.TTFTMillis > 0 {
			ttft = fmt.Sprintf("%dms", r.TTFTMillis)
		}
		cost := "$?"
		if r.CostUSD > 0 {
			cost = fmt.Sprintf("$%.4f", r.CostUSD)
		}
		errStr := "-"
		if r.Error != "" {
			errStr = truncate(r.Error, 40)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%dms\t%d\t%d\t%s\t%s\n",
			r.ID,
			r.StartedAt.Format("2006-01-02 15:04:05"),
			r.Model,
			r.Provider,
			r.StatusCode,
			ttft,
			r.TotalMillis,
			r.PromptTokens,
			r.OutputTokens,
			cost,
			errStr,
		)
	}
	return w.Flush()
}

// runShow prints full detail for a single captured request as pretty JSON.
//
//	llm-top show <id> [--sqlite <path>]
func runShow(id, rest string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}
	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	r, err := st.Get(id)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// runDiff renders a unified diff between two captured requests' bodies.
//
//	llm-top diff <idA> <idB> [--sqlite <path>]
func runDiff(idA, idB, rest string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}
	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	a, err := st.Get(idA)
	if err != nil {
		return fmt.Errorf("a: %w", err)
	}
	b, err := st.Get(idB)
	if err != nil {
		return fmt.Errorf("b: %w", err)
	}
	fmt.Printf("--- request %s\n+++ request %s\n", a.ID, b.ID)
	printUnified(a.RequestBody, b.RequestBody, "request_body")
	fmt.Printf("\n--- response %s\n+++ response %s\n", a.ID, b.ID)
	printUnified(a.ResponseBody, b.ResponseBody, "response_body")
	return nil
}

func printUnified(a, b, label string) {
	for _, l := range diffLines(a, b, label) {
		fmt.Println(l)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func diffLines(a, b, label string) []string {
	as := strings.Split(a, "\n")
	bs := strings.Split(b, "\n")
	out := []string{fmt.Sprintf("@@ %s @@", label)}
	n, m := len(as), len(bs)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if as[i-1] == bs[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	type op struct {
		tag byte
		txt string
	}
	ops := []op{}
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && as[i-1] == bs[j-1]:
			ops = append(ops, op{' ', as[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			ops = append(ops, op{'+', bs[j-1]})
			j--
		default:
			ops = append(ops, op{'-', as[i-1]})
			i--
		}
	}
	for x, y := 0, len(ops)-1; x < y; x, y = x+1, y-1 {
		ops[x], ops[y] = ops[y], ops[x]
	}
	for _, o := range ops {
		out = append(out, fmt.Sprintf("%c %s", o.tag, o.txt))
	}
	return out
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
