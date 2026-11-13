# Changelog

## Unreleased

- Module path is now `github.com/aniketkarne/llm-top` (was
  `github.com/aniketkarne-com/llm-top`) — matches where the repo actually
  lives. `go install`, GoReleaser, badges, and links all follow. Install
  via `@main` until the first release after this change; then `@latest`.
- Request IDs are strictly monotonic per process, and the SQLite
  request/session listings tie-break on descending ID when timestamps
  collide, so "newest first" is deterministic instead of depending on the
  SQL sorter's unspecified tie order.
- Pricing table re-verified 2026-11-13 against the OpenAI and Anthropic
  price sheets: gpt-6-luna, gpt-6.1-terra, gpt-6.1-sol, claude-haiku-5-5,
  claude-sonnet-5-5, claude-opus-5-5. Stale rows (gpt-5.5, gpt-5.5-mini,
  claude-sonnet-5) removed — unknown models render "$?" instead of a
  stale price. Extend via `--pricing` or `Table.With`.
- Compare defaults and examples use current model ids. `LLMTOP_TARGET_GPT`
  replaces `LLMTOP_TARGET_GPT55` (legacy spelling still accepted).
- CI runs the sqlite-tagged store tests (they never ran before) and tests
  on Go 1.27.x + stable to match the go.mod directive.
- README test counts and install/download instructions corrected to match
  reality (287 tests across 16 packages; release assets are macOS arm64).
