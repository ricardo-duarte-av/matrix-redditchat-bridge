#!/bin/sh
# CGO is required for the Matrix-side end-to-bridge encryption support in mautrix-go.
# Build with CGO_ENABLED=0 or -tags nocrypto if you don't need it.
export CGO_ENABLED=${CGO_ENABLED:-1}

MAIN_PKG=./cmd/matrix-redditchat
GO_LDFLAGS="-s -w \
    -X main.Tag=$(git describe --exact-match --tags 2>/dev/null) \
    -X main.Commit=$(git rev-parse HEAD) \
    -X 'main.BuildTime=$(date -Iseconds)'"

go build -ldflags="$GO_LDFLAGS" -o matrix-redditchat "$@" "$MAIN_PKG"
