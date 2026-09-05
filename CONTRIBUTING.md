# Contributing

## License

MIT. By contributing, you agree your contributions are licensed under the same.

## Setup

```bash
go test ./...
go test -race ./...
go build ./cmd/llm-top
```

Requires Go 1.21+.

## Pull requests

- One feature or fix per PR. Avoid bundling unrelated changes.
- Add tests. The repo targets ≥100 passing tests, and the new code should
  add to that count, not subtract.
- Run `gofmt -w .` and `go vet ./...` before pushing.
- Update README.md if your change affects the user-facing surface.

## Filing issues

Open issues on GitHub. For security-sensitive reports, see SECURITY.md.

## Code style

- Idiomatic Go. Follow Effective Go and the Go Code Review Comments.
- Prefer small, focused functions. Avoid clever code.
- Add a doc comment to every exported name. The package-level doc
  comment should explain what the package does in 2-3 sentences.
- Don't add third-party dependencies unless absolutely necessary. The
  whole point of llm-top is being a tiny, drop-in binary.