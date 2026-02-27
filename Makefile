BINARY := clank-agent
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w \
	-X github.com/anaremore/clank/apps/agent/cmd.Version=$(VERSION) \
	-X github.com/anaremore/clank/apps/agent/cmd.Commit=$(COMMIT) \
	-X github.com/anaremore/clank/apps/agent/cmd.Date=$(DATE)

.PHONY: build test clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./... -v

clean:
	rm -f $(BINARY)

# Cross-compilation targets
build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-amd64 .

build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-darwin-arm64 .
