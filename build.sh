#!/usr/bin/env bash
# Build the clank-agent binary with correct version injection.
#
# Usage:
#   bash apps/agent/build.sh                    # linux/amd64 (default)
#   GOOS=linux GOARCH=arm64 bash apps/agent/build.sh   # linux/arm64
#
# Version is read from install-files/VERSION (single source of truth).
# The ldflags target is github.com/anaremore/clank/apps/agent/cmd.Version
# (NOT main.version — the variable lives in the cmd package).
#
# IMPORTANT: Always use this script instead of raw `go build`.
# Raw `go build` on Windows produces a Windows PE binary and version="dev".

set -euo pipefail
cd "$(dirname "$0")"

VERSION=$(tr -d '[:space:]' < ../../install-files/VERSION)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS="-s -w \
  -X github.com/anaremore/clank/apps/agent/cmd.Version=${VERSION} \
  -X github.com/anaremore/clank/apps/agent/cmd.Commit=${COMMIT} \
  -X github.com/anaremore/clank/apps/agent/cmd.Date=${DATE}"

TARGET_OS=${GOOS:-linux}
TARGET_ARCH=${GOARCH:-amd64}

echo "  Building clank-agent v${VERSION} (${COMMIT}) for ${TARGET_OS}/${TARGET_ARCH}..."
CGO_ENABLED=0 GOOS=$TARGET_OS GOARCH=$TARGET_ARCH go build -ldflags "${LDFLAGS}" -o "../../install-files/clank-agent" .
echo "  Done → install-files/clank-agent"
