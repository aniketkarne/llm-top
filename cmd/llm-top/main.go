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
	"github.com/aniketkarne-com/llm-top/internal/compare"
	"github.com/aniketkarne-com/llm-top/internal/config"
	"github.com/aniketkarne-com/llm-top/internal/demo"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
	"github.com/aniketkarne-com/llm-top/internal/pricing"
	"github.com/aniketkarne-com/llm-top/internal/proxy"
	"github.com/aniketkarne-com/llm-top/internal/redactor"
	"github.com/aniketkarne-com/llm-top/internal/replay"
	"github.com/aniketkarne-com/llm-top/internal/session"
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

// replayExitCode carries the desired process exit code out of
// runReplay for the main wrapper to honor. We use a package-level
// variable rather than a second return value because run()'s
// signature is fixed at (args []string) error.
var replayExitCode int
var compareExitCode int

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
				return fmt.Errorf("usage: llm-top diff <id-a> <id-b> [--metrics] [--sqlite <path>]")
			}
			return runDiff(rest[0], rest[1], strings.Join(rest[2:], " "))
		case "compare":
			rest := filtered[1:]
			if len(rest) < 1 || strings.HasPrefix(rest[0], "-") {
				return fmt.Errorf("usage: llm-top compare <request-id> [--target name=url|key,...] [--model X] [--local] [--timeout 30s] [--concurrency N] [--format text|json] [--sqlite <path>]")
			}
			err := runCompare(rest[0], strings.Join(rest[1:], " "))
			if compareExitCode != 0 {
				os.Exit(compareExitCode)
			}
			return err
		case "replay":
			rest := filtered[1:]
			if len(rest) < 1 || strings.HasPrefix(rest[0], "-") {
				return fmt.Errorf("usage: llm-top replay <request-id> [--model X] [--upstream URL] [--api-key KEY] [--local] [--no-stream] [--timeout 30s] [--headers k=v,...] [--sqlite <path>]")
			}
			err := runReplay(rest[0], strings.Join(rest[1:], " "))
			if replayExitCode != 0 {
				os.Exit(replayExitCode)
			}
			return err
		case "curl":
			rest := filtered[1:]
			if len(rest) < 1 || strings.HasPrefix(rest[0], "-") {
				return fmt.Errorf("usage: llm-top curl <request-id> [--reveal-key] [--upstream URL] [--api-key KEY] [--sqlite <path>]")
			}
			return runCurl(rest[0], strings.Join(rest[1:], " "))
		case "edit":
			rest := filtered[1:]
			if len(rest) < 1 || strings.HasPrefix(rest[0], "-") {
				return fmt.Errorf("usage: llm-top edit <request-id> [--upstream URL] [--api-key KEY] [--local] [--timeout 30s] [--sqlite <path>]")
			}
			err := runEdit(rest[0], strings.Join(rest[1:], " "))
			if replayExitCode != 0 {
				os.Exit(replayExitCode)
			}
			return err
		case "session":
			rest := filtered[1:]
			if len(rest) < 1 {
				return fmt.Errorf("usage: llm-top session <start|list|show|diff|stop> ... (try `llm-top help`)")
			}
			switch rest[0] {
			case "start":
				return runSessionStart(strings.Join(rest[1:], " "))
			case "list":
				return runSessionList(strings.Join(rest[1:], " "))
			case "show":
				if len(rest) < 2 {
					return fmt.Errorf("usage: llm-top session show <id|name> [--sqlite PATH]")
				}
				return runSessionShow(rest[1], strings.Join(rest[2:], " "))
			case "diff":
				if len(rest) < 3 {
					return fmt.Errorf("usage: llm-top session diff <a> <b> [--sqlite PATH]")
				}
				return runSessionDiff(rest[1], rest[2], strings.Join(rest[3:], " "))
			case "stop":
				return runSessionStop(strings.Join(rest[1:], " "))
			default:
				return fmt.Errorf("unknown session subcommand %q (try start|list|show|diff|stop)", rest[0])
			}
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
			SessionID:       cfg.SessionID,
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

// runReplay re-fires a captured request against a configured upstream
// (or the captured upstream by default) and prints a one-line summary
// followed by the response body. Exit codes:
//
//	0  request succeeded (2xx)
//	1  transport error or non-2xx status
//	2  missing/invalid arguments or missing sqlite store
func runReplay(id, rest string) error {
	replayExitCode = 0
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	model := fs.String("model", "", "override the model field in the request body")
	upstream := fs.String("upstream", "", "override the upstream base URL")
	apiKey := fs.String("api-key", "", "override the upstream API key")
	local := fs.Bool("local", false, "use http://localhost:11434/v1 (Ollama) as the upstream with no auth")
	noStream := fs.Bool("no-stream", false, "force the request to non-streaming")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	headersRaw := fs.String("headers", "", "comma-separated extra headers (k=v,k=v)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}

	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	captured, err := st.Get(id)
	if err != nil {
		return err
	}

	cfg := replay.ExecutionConfig{
		Model: *model,
	}
	if *local {
		cfg.UpstreamBaseURL = "http://localhost:11434/v1"
		cfg.UpstreamAPIKey = ""
	} else {
		cfg.UpstreamBaseURL = *upstream
		if cfg.UpstreamBaseURL == "" {
			cfg.UpstreamBaseURL = captured.Upstream
		}
		cfg.UpstreamAPIKey = *apiKey
		if cfg.UpstreamAPIKey == "" {
			cfg.UpstreamAPIKey = os.Getenv("LLMTOP_API_KEY")
		}
		if cfg.UpstreamAPIKey == "" {
			cfg.UpstreamAPIKey = os.Getenv("OPENAI_API_KEY")
		}
	}
	cfg.Timeout = *timeout
	if *noStream {
		f := false
		cfg.Stream = &f
	}
	if *headersRaw != "" {
		cfg.Headers = parseHeaderList(*headersRaw)
	}

	res := replay.Execute(context.Background(), captured, cfg)
	fmt.Println(replay.Summary(res))
	body := replay.TruncateBody(res.ResponseBody, 4096)
	if body != "" {
		fmt.Println()
		fmt.Println(body)
	}
	if res.Error != "" {
		replayExitCode = 1
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		replayExitCode = 1
	}
	return nil
}

// runCurl renders a ready-to-paste curl invocation for a captured
// request. By default the Authorization header contains the literal
// $LLMTOP_API_KEY so the user can paste-and-run without leaking.
//
//	llm-top curl <id> [--reveal-key] [--upstream URL] [--api-key KEY] [--sqlite <path>]
func runCurl(id, rest string) error {
	fs := flag.NewFlagSet("curl", flag.ContinueOnError)
	reveal := fs.Bool("reveal-key", false, "substitute a real API key instead of $LLMTOP_API_KEY")
	upstream := fs.String("upstream", "", "override the upstream base URL")
	apiKey := fs.String("api-key", "", "API key to use when --reveal-key is set")
	model := fs.String("model", "", "override the model field in the request body")
	noStream := fs.Bool("no-stream", false, "force the request to non-streaming")
	headersRaw := fs.String("headers", "", "comma-separated extra headers (k=v,k=v)")
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}

	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	captured, err := st.Get(id)
	if err != nil {
		return err
	}

	opts := replay.CurlOptions{
		UpstreamBaseURL: *upstream,
		APIKey:          *apiKey,
		RevealKey:       *reveal,
		Model:           *model,
	}
	if *noStream {
		f := false
		opts.Stream = &f
	}
	if *headersRaw != "" {
		opts.ExtraHeaders = parseHeaderList(*headersRaw)
	}
	if *reveal && opts.APIKey == "" {
		// Fall back to env so reveal-key "just works" when the user
		// has a key in their environment.
		if v := os.Getenv("LLMTOP_API_KEY"); v != "" {
			opts.APIKey = v
		} else if v := os.Getenv("OPENAI_API_KEY"); v != "" {
			opts.APIKey = v
		}
	}
	fmt.Println(replay.CurlCommand(captured, opts))
	return nil
}

// runEdit opens the captured request body in $EDITOR, replays on save,
// and prints the result like `replay` does. Exit codes:
//
//	0  request succeeded
//	1  transport or non-2xx
//	2  invalid JSON after edit (no request fired)
func runEdit(id, rest string) error {
	replayExitCode = 0
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	upstream := fs.String("upstream", "", "override the upstream base URL")
	apiKey := fs.String("api-key", "", "override the upstream API key")
	local := fs.Bool("local", false, "use http://localhost:11434/v1 (Ollama)")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}

	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	captured, err := st.Get(id)
	if err != nil {
		return err
	}

	ed, err := replay.Edit(captured)
	if errors.Is(err, replay.ErrInvalidJSON) {
		fmt.Fprintln(os.Stderr, "llm-top:", err, "(temp file:", ed.Path+")")
		replayExitCode = 2
		return nil
	}
	if err != nil && ed.Skipped {
		fmt.Fprintln(os.Stderr, "llm-top:", err)
	}
	if ed.Body == "" {
		return fmt.Errorf("edit produced empty body")
	}

	cfg := replay.ExecutionConfig{
		Body:    ed.Body,
		Timeout: *timeout,
	}
	if *local {
		cfg.UpstreamBaseURL = "http://localhost:11434/v1"
	} else if *upstream != "" {
		cfg.UpstreamBaseURL = *upstream
	} else {
		cfg.UpstreamBaseURL = captured.Upstream
	}
	cfg.UpstreamAPIKey = *apiKey
	if cfg.UpstreamAPIKey == "" {
		cfg.UpstreamAPIKey = os.Getenv("LLMTOP_API_KEY")
	}

	res := replay.Execute(context.Background(), captured, cfg)
	fmt.Println(replay.Summary(res))
	body := replay.TruncateBody(res.ResponseBody, 4096)
	if body != "" {
		fmt.Println()
		fmt.Println(body)
	}
	if res.Error != "" {
		replayExitCode = 1
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		replayExitCode = 1
	}
	return nil
}

// parseHeaderList parses a "k=v,k=v" flag value into a map. Empty
// pairs and missing '=' are silently dropped — this is a power-user
// flag and we'd rather render nothing than error on a typo.
func parseHeaderList(s string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(p[:eq])
		v := strings.TrimSpace(p[eq+1:])
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// runDiff renders a unified diff between two captured requests'
// bodies. With --metrics, it instead prints a side-by-side per-request
// metric delta (TTFT, total, tokens, cost).
//
//	llm-top diff <idA> <idB> [--metrics] [--sqlite <path>]
func runDiff(idA, idB, rest string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	metricsFlag := fs.Bool("metrics", false, "print side-by-side per-request metric deltas instead of body diff")
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
	if *metricsFlag {
		printMetricsDiff(a, b)
		return nil
	}
	fmt.Printf("--- request %s\n+++ request %s\n", a.ID, b.ID)
	printUnified(a.RequestBody, b.RequestBody, "request_body")
	fmt.Printf("\n--- response %s\n+++ response %s\n", a.ID, b.ID)
	printUnified(a.ResponseBody, b.ResponseBody, "response_body")
	return nil
}

// printMetricsDiff renders a side-by-side metric comparison between two
// captured requests. The output is plain text (no color) so it diffs
// cleanly under version control and works in non-TTY captures.
func printMetricsDiff(a, b proxy.Request) {
	row := func(label, av, bv string) string {
		return fmt.Sprintf("%-14s %14s   %14s\n", label, av, bv)
	}
	fmt.Printf("metric              %14s   %14s\n", a.ID, b.ID)
	fmt.Println(strings.Repeat("─", 46))
	costA, costB := "$?", "$?"
	if a.CostUSD > 0 {
		costA = fmt.Sprintf("$%.4f", a.CostUSD)
	}
	if b.CostUSD > 0 {
		costB = fmt.Sprintf("$%.4f", b.CostUSD)
	}
	ttftA, ttftB := "-", "-"
	if a.TTFTMillis > 0 {
		ttftA = fmt.Sprintf("%dms", a.TTFTMillis)
	}
	if b.TTFTMillis > 0 {
		ttftB = fmt.Sprintf("%dms", b.TTFTMillis)
	}
	fmt.Print(row("model", a.Model, b.Model))
	fmt.Print(row("upstream", a.Upstream, b.Upstream))
	fmt.Print(row("status", fmt.Sprintf("%d", a.StatusCode), fmt.Sprintf("%d", b.StatusCode)))
	fmt.Print(row("stream", fmt.Sprintf("%t", a.Stream), fmt.Sprintf("%t", b.Stream)))
	fmt.Print(row("ttft", ttftA, ttftB))
	fmt.Print(row("total", fmt.Sprintf("%dms", a.TotalMillis), fmt.Sprintf("%dms", b.TotalMillis)))
	fmt.Print(row("in_tokens", fmt.Sprintf("%d", a.PromptTokens), fmt.Sprintf("%d", b.PromptTokens)))
	fmt.Print(row("out_tokens", fmt.Sprintf("%d", a.OutputTokens), fmt.Sprintf("%d", b.OutputTokens)))
	fmt.Print(row("cost", costA, costB))
	errA, errB := "-", "-"
	if a.Error != "" {
		errA = truncate(a.Error, 30)
	}
	if b.Error != "" {
		errB = truncate(b.Error, 30)
	}
	fmt.Print(row("error", errA, errB))
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

// runCompare re-fires a captured request against one or more targets in
// parallel and prints the side-by-side table.
//
//	llm-top compare <id>
//	  [--target "name=baseurl|apikey,name=baseurl|apikey,..."] (repeatable)
//	  [--model X]                   override model on all targets
//	  [--local]                     add http://localhost:11434/v1 as 'local'
//	  [--timeout 30s]               per-target timeout
//	  [--concurrency N]             max parallel targets (default = len(targets))
//	  [--format text|json]
//	  [--sqlite PATH]
func runCompare(id, rest string) error {
	compareExitCode = 0 // reset on entry

	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	targetList := fs.String("target", "", "comma-separated list of name=url|apikey targets")
	model := fs.String("model", "", "override model on all targets")
	local := fs.Bool("local", false, "add http://localhost:11434/v1 as 'local' target")
	timeout := fs.Duration("timeout", 30*time.Second, "per-target timeout")
	concurrency := fs.Int("concurrency", 0, "max parallel targets (default = all)")
	format := fs.String("format", "text", "output format: text or json")
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}

	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	captured, err := st.Get(id)
	if err != nil {
		return err
	}

	targets, err := buildCompareTargets(*targetList, *local, *model)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no targets configured: use --target or --local (and set OPENAI_API_KEY / ANTHROPIC_API_KEY env vars for defaults)")
	}

	result := compare.Run(context.Background(), captured, targets, compare.Options{
		Timeout:     *timeout,
		Concurrency: *concurrency,
		Pricing:     pricing.Defaults(),
	})

	switch *format {
	case "json":
		if err := compare.RenderJSON(os.Stdout, result); err != nil {
			return err
		}
	default:
		compare.RenderText(os.Stdout, captured.ID, result.Rows)
	}

	// Exit code: 0 if any 2xx, 1 if all targets failed.
	anyOK := false
	for _, r := range result.Rows {
		if r.StatusCode >= 200 && r.StatusCode < 300 && r.Error == "" {
			anyOK = true
			break
		}
	}
	if !anyOK {
		compareExitCode = 1
	}
	return nil
}

// buildCompareTargets parses the --target flag (repeatable comma-separated)
// and appends --local if requested. Each target string is "name=baseurl|apikey".
// If no --target is given, falls back to env-driven defaults so the user can
// `llm-top compare <id> --local` and get something useful immediately.
func buildCompareTargets(targetFlag string, local bool, modelOverride string) ([]compare.Target, error) {
	var targets []compare.Target
	if targetFlag != "" {
		for _, raw := range strings.Split(targetFlag, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			t, err := parseCompareTarget(raw, modelOverride)
			if err != nil {
				return nil, err
			}
			targets = append(targets, t)
		}
	} else {
		// Env defaults so `compare <id> --local` does something useful
		// even without any --target flags.
		if v := os.Getenv("LLMTOP_TARGET_GPT55"); v != "" {
			t, err := parseCompareTarget("gpt-5.5="+v, modelOverride)
			if err != nil {
				return nil, err
			}
			targets = append(targets, t)
		}
		if v := os.Getenv("LLMTOP_TARGET_CLAUDE"); v != "" {
			t, err := parseCompareTarget("claude-sonnet-5="+v, modelOverride)
			if err != nil {
				return nil, err
			}
			targets = append(targets, t)
		}
		// Built-in defaults: openai / anthropic if their env keys are set.
		if len(targets) == 0 {
			if k := os.Getenv("OPENAI_API_KEY"); k != "" {
				targets = append(targets, compare.Target{
					Name: "gpt-5.5", BaseURL: "https://api.openai.com/v1", APIKey: k, Model: "gpt-5.5",
				})
			}
			if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
				targets = append(targets, compare.Target{
					Name: "claude-sonnet-5", BaseURL: "https://api.anthropic.com/v1", APIKey: k, Model: "claude-sonnet-5",
				})
			}
		}
	}
	if local {
		targets = append(targets, compare.Target{
			Name: "local", BaseURL: "http://localhost:11434/v1", Model: "", // empty = use captured model
		})
	}
	// Apply model override uniformly if requested and the target didn't set one.
	if modelOverride != "" {
		for i := range targets {
			if targets[i].Model == "" {
				targets[i].Model = modelOverride
			}
		}
	}
	return targets, nil
}

// parseCompareTarget parses "name=baseurl|apikey" into a compare.Target.
// The "apikey" part is optional (empty for local targets).
func parseCompareTarget(raw, modelOverride string) (compare.Target, error) {
	eq := strings.IndexByte(raw, '=')
	if eq < 0 {
		return compare.Target{}, fmt.Errorf("target %q: missing '=' (expected name=baseurl|apikey)", raw)
	}
	name := strings.TrimSpace(raw[:eq])
	rest := raw[eq+1:]
	var baseURL, apiKey string
	if pipe := strings.IndexByte(rest, '|'); pipe >= 0 {
		baseURL = strings.TrimSpace(rest[:pipe])
		apiKey = strings.TrimSpace(rest[pipe+1:])
	} else {
		baseURL = strings.TrimSpace(rest)
	}
	if name == "" {
		return compare.Target{}, fmt.Errorf("target %q: empty name", raw)
	}
	if baseURL == "" {
		return compare.Target{}, fmt.Errorf("target %q: empty base URL", raw)
	}
	return compare.Target{
		Name:    name,
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   modelOverride, // empty here means "use captured model"
	}, nil
}

// --- session subcommands ---

// runSessionStart creates a new Session and prints its id. Until the
// user runs `session stop` or starts a new session, captured requests
// can be attached to it via the proxy --session flag.
//
//	llm-top session start [--name X] [--label Y] [--tag T] [--sqlite PATH]
func runSessionStart(rest string) error {
	fs := flag.NewFlagSet("session-start", flag.ContinueOnError)
	name := fs.String("name", "", "optional human-readable name")
	label := fs.String("label", "", "optional one-word label")
	tagList := fs.String("tag", "", "comma-separated tags (repeatable)")
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}
	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	mgr := session.NewManager(st)
	tags := []string{}
	if *tagList != "" {
		for _, t := range strings.Split(*tagList, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}
	sess, err := mgr.Create(session.Session{
		Name:  *name,
		Label: *label,
		Tags:  tags,
	})
	if err != nil {
		return err
	}
	fmt.Printf("session started: id=%s name=%q label=%q started=%s\n",
		sess.ID, sess.Name, sess.Label, sess.StartedAt.Format(time.RFC3339))
	if *name != "" || *label != "" || len(tags) > 0 {
		fmt.Printf("hint: start the proxy with --session %s (or LLMTOP_SESSION=%s) to attach captured requests\n", sess.ID, sess.ID)
	}
	return nil
}

// runSessionList prints all sessions, newest first.
//
//	llm-top session list [--limit N] [--sqlite PATH]
func runSessionList(rest string) error {
	fs := flag.NewFlagSet("session-list", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "max sessions to show")
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}
	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	mgr := session.NewManager(st)
	sessions, err := mgr.List()
	if err != nil {
		return err
	}
	if *limit > 0 && len(sessions) > *limit {
		sessions = sessions[:*limit]
	}
	if len(sessions) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions in database")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID	NAME	LABEL	STARTED	ENDED	ACTIVE")
	for _, s := range sessions {
		ended := "-"
		if !s.EndedAt.IsZero() {
			ended = s.EndedAt.Format("2006-01-02 15:04:05")
		}
		active := "no"
		if s.Active {
			active = "yes"
		}
		fmt.Fprintf(w, "%s	%s	%s	%s	%s	%s\n",
			s.ID,
			s.Name,
			s.Label,
			s.StartedAt.Format("2006-01-02 15:04:05"),
			ended,
			active,
		)
	}
	return w.Flush()
}

// runSessionShow prints one session's metadata + aggregate stats over
// its linked requests.
//
//	llm-top session show <id|name> [--sqlite PATH]
func runSessionShow(ref, rest string) error {
	fs := flag.NewFlagSet("session-show", flag.ContinueOnError)
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}
	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	mgr := session.NewManager(st)
	sess, err := resolveSession(mgr, ref)
	if err != nil {
		return err
	}
	listFn := func(sessionID string) ([]proxy.Request, error) {
		return st.List(store.ListFilter{SessionID: sessionID})
	}
	agg, err := mgr.Aggregate(sess.ID, listFn)
	if err != nil {
		return err
	}
	// Fill the Session struct's summary fields for the renderer.
	sess.RequestCount = agg.Count
	sess.ErrorCount = agg.ErrorCount
	sess.TotalCostUSD = agg.TotalCostUSD
	sess.TotalTokens = agg.TotalInputTokens + agg.TotalOutputTokens
	session.RenderShow(os.Stdout, sess, agg)
	return nil
}

// runSessionDiff prints the side-by-side delta between two sessions.
//
//	llm-top session diff <a> <b> [--sqlite PATH]
func runSessionDiff(a, b, rest string) error {
	fs := flag.NewFlagSet("session-diff", flag.ContinueOnError)
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}
	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	mgr := session.NewManager(st)
	aSess, err := resolveSession(mgr, a)
	if err != nil {
		return fmt.Errorf("a: %w", err)
	}
	bSess, err := resolveSession(mgr, b)
	if err != nil {
		return fmt.Errorf("b: %w", err)
	}
	listFn := func(sessionID string) ([]proxy.Request, error) {
		return st.List(store.ListFilter{SessionID: sessionID})
	}
	aAgg, err := mgr.Aggregate(aSess.ID, listFn)
	if err != nil {
		return err
	}
	bAgg, err := mgr.Aggregate(bSess.ID, listFn)
	if err != nil {
		return err
	}
	session.RenderDiff(os.Stdout, aSess, aAgg, bSess, bAgg)
	return nil
}

// runSessionStop ends the currently-active session (or the one named
// via --id). Prints the stopped id and exit time.
//
//	llm-top session stop [--id ID] [--sqlite PATH]
func runSessionStop(rest string) error {
	fs := flag.NewFlagSet("session-stop", flag.ContinueOnError)
	id := fs.String("id", "", "session id to stop (default: the currently-active session)")
	sqlitePath := fs.String("sqlite", "", "sqlite db path (default LLMTOP_SQLITE or ./llm-top.db)")
	if err := fs.Parse(strings.Fields(rest)); err != nil {
		return err
	}
	st, cleanup, err := openCLIStore(*sqlitePath)
	if err != nil {
		return err
	}
	defer cleanup()
	mgr := session.NewManager(st)
	target := *id
	if target == "" {
		active, err := mgr.Active()
		if err != nil {
			return fmt.Errorf("no --id provided and no active session: %w", err)
		}
		target = active.ID
	}
	if err := mgr.End(target, time.Now().UTC()); err != nil {
		return err
	}
	fmt.Printf("session stopped: id=%s ended=%s\n", target, time.Now().UTC().Format(time.RFC3339))
	return nil
}

// resolveSession looks up a session by 16-hex id first, then by
// case-sensitive name. Returns ErrNotFound when neither resolves.
func resolveSession(mgr *session.Manager, ref string) (session.Session, error) {
	if s, err := mgr.Get(ref); err == nil {
		return s, nil
	}
	return mgr.FindByName(ref)
}
