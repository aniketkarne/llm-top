<p align="center">
  <img src="assets/llm-top-logo.jpg" alt="llm-top" width="160">
</p>

<p align="center">
  <a href="https://github.com/aniketkarne-com/llm-top/releases/latest"><img src="https://img.shields.io/github/v/release/aniketkarne-com/llm-top?style=flat-square&label=release" alt="Release"></a>
  <a href="https://github.com/aniketkarne-com/llm-top/actions/workflows/tests.yml"><img src="https://img.shields.io/github/actions/workflow/status/aniketkarne-com/llm-top/tests.yml?branch=main&style=flat-square&label=tests" alt="Tests"></a>
  <a href="https://github.com/aniketkarne-com/llm-top/blob/main/LICENSE"><img src="https://img.shields.io/github/license/aniketkarne-com/llm-top?style=flat-square" alt="License: MIT"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-1.21%2B-00ADD8?style=flat-square&logo=go" alt="Go 1.21+"></a>
</p>

<p align="center">
  <b>101 tests pass.</b> Single static binary. <b>Zero dependencies</b> at runtime — just point your code at the local proxy and watch every LLM call live.
</p>

<img width="1146" height="569" alt="image" src="https://github.com/user-attachments/assets/faef2d1a-2e59-449a-94c3-6e21a5d23f52" />

---

# llm-top

> OpenAI-compatible HTTP reverse proxy with a built-in terminal dashboard,
> SSE streaming interception, TTFT/latency/token metrics, and prompt capture.

You write an Agent. You give it a loop. You open your editor. Your prompt
spends **$0.30** on a request that streams for **8 seconds** and you have
no idea why. You switch to a browser dashboard that's slow. You open
Postman to inspect a single call. You lose your place.

**llm-top** is a single Go binary you point your code at. Every LLM call
becomes a live row in a terminal dashboard — TTFT, tokens/sec, prompts,
responses, status codes — and stays there for the last 500 requests with
secrets already redacted. Zero infrastructure. Zero database. No "create
an account, paste your API key, wait for indexing."

```text
╔══════════════════════════════════════════════════════════════════════════╗
║                            llm-top  architecture                          ║
╚══════════════════════════════════════════════════════════════════════════╝

  ┌────────────┐    HTTP     ┌───────────────────────┐     HTTP/SSE    ┌──────────────┐
  │  client    │  ────────►  │  llm-top  (Go)        │  ────────────►  │  upstream    │
  │ (your app) │  ◄────────  │  ┌─────────────────┐  │  ◄────────────  │  (OpenAI,    │
  │  curl, SDK │   SSE/JSON  │  │ reverse proxy   │  │   SSE/JSON     │   vLLM, ...) │
  └────────────┘             │  │  + SSE parser   │  │                └──────────────┘
                             │  │  + metrics      │  │
                             │  │  + redactor     │  │   ┌──────────────────────────┐
                             │  └────────┬────────┘  │   │   terminal UI (TUI)      │
                             │           │           │   │  ┌─────────────────────┐  │
                             │           ▼           │──►│  │ metrics  │ activity │  │
                             │  ┌─────────────────┐  │   │  │ p50/p95 │  ring    │  │
                             │  │  circular ring  │  │   │  │ TTFT    │  buffer  │  │
                             │  │  (max 500)      │  │   │  └─────────────────────┘  │
                             │  └─────────────────┘  │   └──────────────────────────┘
                             │           │
                             │           ▼   (optional, build tag `sqlite`)
                             │  ┌─────────────────┐
                             │  │  sessions.db    │
                             │  └─────────────────┘
                             └───────────────────────┘
```

## Quickstart — 30 seconds

```bash
# 1. Install
go install github.com/aniketkarne-com/llm-top/cmd/llm-top@latest

# 2. Try the zero-deps demo (no API key, no network, ~2s)
llm-top demo

# 3. Run with your real upstream
LLMTOP_UPSTREAM=https://api.openai.com \
  LLMTOP_API_KEY=sk-... \
  llm-top

# 4. Point your client at it
OPENAI_BASE_URL=http://127.0.0.1:7777 OPENAI_API_KEY=anything curl \
  http://127.0.0.1:7777/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

You should see live requests, TTFT, token counts, and the redacted prompt /
response scroll through the TUI. Watch the "avg TTFT" settle within 10 calls.

## Features

- **OpenAI-compatible HTTP reverse proxy.** Point any OpenAI SDK at it; the
  upstream base URL is configurable. Works with OpenAI, Azure, vLLM, Ollama,
  Together, Groq, and any other OpenAI-shaped endpoint.
- **SSE streaming interception.** Parses `text/event-stream` byte-for-byte,
  measures time-to-first-token, accumulates the assistant text, and forwards
  events to the client with per-event flushing.
- **Per-request metrics.** TTFT, total latency, prompt tokens (estimated),
  output tokens (estimated from JSON `usage` or text length), model, status,
  stream vs. non-stream, p50/p95/p99 aggregates.
- **Circular buffer capture.** Fixed-size ring (default 500) of recent
  requests and responses, redacted of obvious secrets.
- **Built-in terminal UI.** Single-binary TUI with metrics pane + activity
  pane + status bar. No external bubbletea/lipgloss dependency; ANSI colors
  with a graceful non-TTY fallback.
- **Zero-deps demo.** `llm-top demo` runs an end-to-end showcase — mock
  OpenAI server, real proxy, real TUI snapshot — with no API key and no
  network calls. First-run experience.
- **Optional SQLite session dump.** Persist captured events to a SQLite DB
  for post-mortem inspection. Pure-Go (`modernc.org/sqlite`) — no CGO.
  Compile with `-tags sqlite` to enable; absent that, the store is a no-op.
- **Graceful shutdown** on SIGINT/SIGTERM with bounded quiescence.
- **Secret redaction.** Bearer tokens, OpenAI keys, JWTs, generic
  `password=` pairs, `x-api-key` headers — all scrubbed before anything
  is written to the ring buffer or the SQLite dump. Upstream traffic to
  the real API is never redacted.

## Install

```bash
# Recommended: go install (requires Go 1.21+)
go install github.com/aniketkarne-com/llm-top/cmd/llm-top@latest

# macOS / Linux binary download from the latest release:
curl -L https://github.com/aniketkarne-com/llm-top/releases/latest/download/llm-top_$(uname -s)_$(uname -m).tar.gz | tar xz
sudo mv llm-top /usr/local/bin/

# With SQLite session dump enabled:
go install -tags sqlite github.com/aniketkarne-com/llm-top/cmd/llm-top@latest
```

## Configuration

Configuration is loaded in order of precedence: **CLI flags → environment
variables → defaults**.

### Flags

| Flag          | Description                                    | Default                  |
|---------------|------------------------------------------------|--------------------------|
| `--listen`    | Address to listen on                           | `127.0.0.1:7777`         |
| `--upstream`  | Upstream base URL (OpenAI-compatible)          | `https://api.openai.com` |
| `--api-key`   | Upstream API key                               | `$LLMTOP_API_KEY`        |
| `--buffer`    | Circular buffer capacity (max 100000)          | `500`                    |
| `--ui`        | Launch TUI alongside the proxy                 | `true` (integrated)      |
| `--sqlite`    | Optional SQLite session-dump path              | unset (disabled)         |
| `--mode`      | `proxy` \| `ui` \| `integrated`                | `integrated`             |

### Environment variables

```
LLMTOP_LISTEN
LLMTOP_UPSTREAM
LLMTOP_API_KEY       (falls back to $OPENAI_API_KEY)
LLMTOP_BUFFER
LLMTOP_UI            (1/0, true/false)
LLMTOP_SQLITE
LLMTOP_MODE
```

## Subcommands

| Subcommand   | Behavior                                              |
|--------------|-------------------------------------------------------|
| (none)       | Integrated mode: proxy + TUI                          |
| `proxy`      | Run only the HTTP proxy, no TUI                       |
| `ui`         | Run only the TUI (assumes a proxy is reachable)       |
| `integrated` | Explicit alias for the default                        |
| `demo`       | Zero-deps end-to-end showcase                         |
| `version`    | Print version and exit                                |
| `help`       | Usage                                                 |

## SQLite session dump

SQLite support is gated behind the `sqlite` build tag to keep the default
binary small and free of the pure-Go SQLite driver. To enable:

```bash
go build -tags sqlite -o llm-top ./cmd/llm-top
./llm-top --sqlite sessions.db
```

Every ring-buffer entry is mirrored to a `events(id, ts, kind, model, content)`
table. The store is best-effort: a write failure never breaks the proxy.
Without `-tags sqlite`, `--sqlite` is silently ignored.

## Redaction

The redactor scrubs obvious secrets — `Authorization: Bearer ***`,
`sk-…` OpenAI keys, `x-api-key: ***` headers, generic `password=…`
pairs — before anything is written to the ring buffer or the SQLite dump.
Upstream traffic to the real API is never redacted (you want the real
response).

## Limitations — what llm-top is NOT

- **Not an LLM client library.** llm-top is a passive observer — it
  doesn't talk to OpenAI for you. It proxies requests and captures
  metrics. Your existing code works unchanged.
- **Not a tracing/metrics platform.** No Prometheus, OTLP, Datadog, or
  Grafana integration. Counters are local to the running process. For
  long-term observability, use a real APM tool *and* llm-top for the
  dev-loop UX.
- **Not multi-process safe.** The ring buffer is in-memory. If you run
  two llm-top instances on the same machine, they don't share state.
  SQLite gives you a single-process durable record; for cross-process
  observability, federate the SQLite files or use a different tool.
- **Not a streaming protocol normalizer.** llm-top understands OpenAI-
  shaped SSE. Anthropic's `event: message_delta` format works because
  OpenAI's API does too on the wire. If your provider uses a different
  streaming shape, the proxy still works but the per-token preview in
  the ring buffer will be empty.
- **No token pricing tables.** Costs shown are estimate-only. Verify
  with your provider's actual billing.
- **No LLM calls.** llm-top never calls a model. There is no `init()`
  API key requirement, no model download, no warm-up. It is a passive
  HTTP proxy.

## Development

```bash
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
go build -o bin/llm-top ./cmd/llm-top
go build -tags sqlite -o bin/llm-top-sqlite ./cmd/llm-top
```

101 tests across 8 packages cover happy/edge cases for SSE parsing,
redaction, ring buffer eviction, metrics aggregation, the proxy, and the
demo orchestrator. Race detector clean.

## License

MIT.
