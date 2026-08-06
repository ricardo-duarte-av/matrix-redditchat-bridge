FROM golang:1.25-alpine AS builder

# CGO is needed for the sqlite3 driver. The goolm build tag picks mautrix's pure-Go crypto over
# libolm, so no olm C headers are required; encryption is still compiled in, just disabled unless
# turned on in the config.
RUN apk add --no-cache build-base git

WORKDIR /build
# Copy the module files first so dependency downloads stay cached across source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=1 go build -tags goolm -ldflags "-s -w \
        -X main.Commit=${COMMIT} \
        -X 'main.BuildTime=${BUILD_TIME}'" \
    -o /build/matrix-redditchat ./cmd/matrix-redditchat


FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata su-exec

COPY --from=builder /build/matrix-redditchat /usr/local/bin/matrix-redditchat
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Everything the operator needs to keep lives here: config, registrations and the database.
ENV BRIDGE_DATA_DIR=/data
VOLUME /data
WORKDIR /data

EXPOSE 29340

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
