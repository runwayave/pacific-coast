# syntax=docker/dockerfile:1.7

# ---------- build ----------
# Use BUILDPLATFORM so the compiler runs natively on the host (ARM64 on Apple
# Silicon, amd64 on CI). pg_query_go's vendored C parser compiles fine on both
# architectures with musl + build-base. For a forced amd64 production image,
# pass --platform linux/amd64 to docker build or use a CI runner.
FROM --platform=$BUILDPLATFORM golang:1.25.0-alpine3.21 AS build

# CGO toolchain for pg_query_go (vendored C parser, statically linked).
RUN apk add --no-cache build-base

WORKDIR /src

# Cache go mod download. The SDK go.mod is needed because of the local replace.
COPY go.mod go.sum* ./
COPY clients/go/go.mod clients/go/go.sum* ./clients/go/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# VERSION is stamped into the binary (-X main.version) and surfaced in the
# startup log. Pass --build-arg VERSION=<tag-or-sha>; defaults to "dev".
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 \
    go build -ldflags="-s -w -extldflags=-static -X main.version=${VERSION}" \
    -o /out/atlantis ./cmd/server

# ---------- runtime ----------
FROM alpine:3.21

RUN adduser -D -u 10001 atlantis && \
    apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=build /out/atlantis /app/atlantis
COPY migrations /app/migrations

# /app/schema is writable so `tide apply` can mirror submitted .atl files
# when ATL_MIRROR_SCHEMA=true (dev-only — see deploy/.env.example). Owned
# by the non-root atlantis user so the server never runs as root.
RUN mkdir -p /app/schema && chown -R atlantis:atlantis /app

USER atlantis

# 9090 gRPC; 8081 health/metrics (/healthz, /readyz, /metrics).
EXPOSE 9090 8081

# Readiness reflects true serving state (pg + memcached + outbox liveness), so
# a container only reports healthy once it can actually serve. start-period
# covers boot + AUTO_MIGRATE. Orchestrators should still wire the dedicated
# /healthz (liveness) and /readyz (readiness) probes directly rather than rely
# on this single signal. busybox wget exits non-zero on a 503, so no jq needed.
HEALTHCHECK --start-period=20s --interval=15s --timeout=5s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8081/readyz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/atlantis"]
