# llm-top

> OpenAI-compatible HTTP reverse proxy with a built-in terminal dashboard,
> SSE streaming interception, TTFT/latency/token metrics, and prompt capture.

A single Go binary that sits between your code and any OpenAI-compatible
endpoint (OpenAI, Azure, vLLM, Ollama, Together, Groq, ...) and gives you
real-time visibility into every request.

```
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

## Features

- **OpenAI-compatible HTTP reverse proxy.** Point any OpenAI SDK at it; the
  upstream base URL is configurable.
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
- **Optional SQLite session dump.** Persist captured events to a SQLite DB
  for post-mortem inspection. Pure-Go (`modernc.org/sqlite`) — no CGO.
  Compile with `-tags sqlite` to enable; absent that, the store is a no-op.
- **Graceful shutdown** on SIGINT/SIGTERM with bounded quiescence.

## Quickstart (3 steps)

### 1. Build

```sh
go build -o bin/llm-top ./cmd/llm-top
```

### 2. Run with your upstream

```sh
# default: integrated mode (proxy + TUI), listens on 127.0.0.1:7777
LLMTOP_UPSTREAM=https://api.openai.com \
  LLMTOP_API_KEY=sk-... \
  ./bin/llm-top
```

### 3. Point your client at it

```sh
# OpenAI SDK
OPENAI_BASE_URL=http://127.0.0.1:7777 OPENAI_API_KEY=anything curl \
  http://127.0.0.1:7777/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

You should see live requests, TTFT, token counts, and the redacted prompt /
response scroll through the dashboard.

## Configuration

Configuration is loaded in order of precedence: **CLI flags → environment
variables → config file → defaults**.

### Flags

| Flag          | Description                                    | Default                  |
|---------------|------------------------------------------------|--------------------------|
| `--listen`    | Address to listen on                           | `127.0.0.1:7777`         |
| `--upstream`  | Upstream base URL (OpenAI-compatible)          | `https://api.openai.com` |
| `--api-key`   | Upstream API key                               | `$LLMTOP_API_KEY`        |
| `--buffer`    | Circular buffer capacity (max 100000)          | `500`                    |
| `--ui`        | Launch TUI alongside the proxy                 | `true` (integrated)      |
| `--sqlite`    | Optional SQLite session-dump path              | unset (disabled)        |
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

### Config file

JSON or flat-key YAML, passed via `--config`. Re-parsed after flags so it
overrides env+defaults but is itself overridden by explicit CLI flags.

```json
{
  "listen_addr": "127.0.0.1:7777",
  "upstream_base_url": "https://api.openai.com",
  "upstream_api_key": "sk-...",
  "buffer_size": 500,
  "ui": true,
  "sqlite_path": "sessions.db",
  "mode": "integrated"
}
```

```yaml
listen_addr: 127.0.0.1:7777
upstream_base_url: https://api.openai.com
buffer_size: 500
ui: true
```

## Subcommands

| Subcommand   | Behavior                                              |
|--------------|-------------------------------------------------------|
| (none)       | Integrated mode: proxy + TUI                          |
| `proxy`      | Run only the HTTP proxy, no TUI                       |
| `ui`         | Run only the TUI (assumes a proxy is reachable)       |
| `integrated` | Explicit alias for the default                        |
| `version`    | Print version and exit                                |
| `help`       | Usage                                                 |

## SQLite session dump

SQLite support is gated behind the `sqlite` build tag to keep the default
binary small and free of the pure-Go SQLite driver. To enable:

```sh
go build -tags sqlite -o bin/llm-top ./cmd/llm-top
./bin/llm-top --sqlite sessions.db
```

Every ring-buffer entry is mirrored to a `events(id, ts, kind, model, content)`
table. The store is best-effort: a write failure never breaks the proxy.

Without `-tags sqlite`, `--sqlite` is silently ignored.

## Redaction

The redactor scrubs obvious secrets — `Authorization: Bearer …`,
`sk-…` OpenAI keys, `x-api-key: …` headers, JWTs, generic `password=…`
pairs — before anything is written to the ring buffer or the SQLite dump.
Upstream traffic to the real API is never redacted (you want the real
response).

## Development

```sh
gofmt -w .
go vet ./...
go test ./...
go build -o bin/llm-top ./cmd/llm-top
```

## License

MIT