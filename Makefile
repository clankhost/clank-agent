BINARY := clank-agent
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w \
	-X github.com/anaremore/clank/apps/agent/cmd.Version=$(VERSION) \
	-X github.com/anaremore/clank/apps/agent/cmd.Commit=$(COMMIT) \
	-X github.com/anaremore/clank/apps/agent/cmd.Date=$(DATE)

.PHONY: build test clean release-snapshot build-all

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./... -v

clean:
	rm -f $(BINARY)
	rm -rf dist/

release-snapshot:
	goreleaser release --snapshot --clean

# Cross-compilation targets
build-all: build-linux build-linux-arm64 build-darwin-amd64 build-darwin-arm64

build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-amd64 .

build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-arm64 .

build-darwin-amd64:
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-darwin-amd64 .

build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-darwin-arm64 .
