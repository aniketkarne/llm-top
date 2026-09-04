.PHONY: build test vet fmt clean run-sqlite

BIN ?= bin/llm-top

build:
	go build -o $(BIN) ./cmd/llm-top

build-sqlite:
	go build -tags sqlite -o $(BIN) ./cmd/llm-top

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

run-sqlite: build-sqlite
	$(BIN)

clean:
	rm -rf bin/