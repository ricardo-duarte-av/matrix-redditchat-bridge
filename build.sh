#!/bin/sh
# CGO is required for the sqlite3 driver. The goolm build tag selects mautrix's pure-Go crypto
# instead of libolm, so no C olm headers are needed while end-to-bridge encryption still works.
export CGO_ENABLED=${CGO_ENABLED:-1}

MAIN_PKG=./cmd/matrix-redditchat
GO_LDFLAGS="-s -w \
    -X main.Tag=$(git describe --exact-match --tags 2>/dev/null) \
    -X main.Commit=$(git rev-parse HEAD) \
    -X 'main.BuildTime=$(date -Iseconds)'"

go build -tags goolm -ldflags="$GO_LDFLAGS" -o matrix-redditchat "$@" "$MAIN_PKG"
