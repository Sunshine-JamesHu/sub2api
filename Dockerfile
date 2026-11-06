# syntax=docker/dockerfile:1.7
# =============================================================================
# Sub2API Multi-Stage Dockerfile
# =============================================================================
# Stage 1: Build frontend
# Stage 2: Build Go backend with embedded frontend
# Stage 3: Final minimal image
# =============================================================================

ARG NODE_IMAGE=node:24-alpine
ARG GOLANG_IMAGE=golang:1.27.3-alpine
ARG ALPINE_IMAGE=alpine:3.21
ARG POSTGRES_IMAGE=postgres:18-alpine
ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn
ARG NPM_CONFIG_REGISTRY=

# -----------------------------------------------------------------------------
# Stage 1: Frontend Builder
# -----------------------------------------------------------------------------
# --platform=$BUILDPLATFORM: the frontend output is JS (arch-neutral), so build
# it on the native host arch instead of under QEMU emulation for the target.
FROM --platform=${BUILDPLATFORM} ${NODE_IMAGE} AS frontend-builder
ARG NPM_CONFIG_REGISTRY

WORKDIR /app/frontend

# Install pnpm (pinned to v9 to match CI and keep builds reproducible)
RUN corepack enable && corepack prepare pnpm@9 --activate

# Install dependencies first (better caching)
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN --mount=type=cache,id=sub2api-pnpm-store,target=/root/.local/share/pnpm/store \
    if [ -n "${NPM_CONFIG_REGISTRY}" ]; then pnpm config set registry "${NPM_CONFIG_REGISTRY}"; fi && \
    pnpm install --frozen-lockfile --prefer-offline

# Copy frontend source and build.
# LegalDocumentView.vue (admin-compliance gate) build-time imports
# ../../../../docs/legal/*.md?raw, so docs/legal/ must sit beside frontend/
# in the image (WORKDIR /app/frontend -> resolves to /app/docs/legal/*.md).
# Copy only that subtree to keep the build dependency minimal.
COPY frontend/ ./
COPY docs/legal/ /app/docs/legal/
RUN pnpm run build

# -----------------------------------------------------------------------------
# Stage 2: Backend Builder
# -----------------------------------------------------------------------------
# Run Go and Zig on the build host; Zig supplies the target musl/libc++ toolchain
# used by the prebuilt SpiderMonkey library.
FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS backend-builder

# Build arguments for version info (set by CI)
ARG VERSION=
ARG COMMIT=docker
ARG DATE
ARG GOPROXY
ARG GOSUMDB
# Populated by buildx from the --platform target (e.g. linux/amd64).
ARG TARGETOS
ARG TARGETARCH
ARG SPIDERMONKEY_VER=v152.0.4-aistudio2api.1
ARG ZIG_VER=0.16.0

ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}

# Install build dependencies and prebuilt SpiderMonkey static libraries for WAA
RUN apk add --no-cache git ca-certificates tzdata curl xz && \
    mkdir -p /opt/zig && \
    curl -fsSL --retry 3 https://ziglang.org/download/${ZIG_VER}/zig-$(uname -m)-linux-${ZIG_VER}.tar.xz \
    | tar -xJ --strip-components=1 -C /opt/zig && \
    mkdir -p /opt/spidermonkey && \
    curl -fsSL --retry 3 https://github.com/Mag1cFall/firefox/releases/download/${SPIDERMONKEY_VER}/spidermonkey-${SPIDERMONKEY_VER}-linux-${TARGETARCH:-amd64}.tar.gz \
    | tar -xz -C /opt/spidermonkey

WORKDIR /app/backend

# Copy go mod files first (better caching)
COPY backend/go.mod backend/go.sum ./
# Cache mount keeps the module cache across builds so a transient CDN blip on
# retry resumes instead of re-fetching every zip from scratch.
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    go mod download

# Copy backend source first
COPY backend/ ./

# Copy frontend dist from previous stage (must be after backend copy to avoid being overwritten)
COPY --from=frontend-builder /app/backend/internal/web/dist ./internal/web/dist

# Build the binary (BuildType=release for CI builds, embed frontend)
# Version precedence: build arg VERSION > exact git tag > cmd/server/VERSION
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=sub2api-gobuild,target=/root/.cache/go-build \
    --mount=type=cache,id=sub2api-zig,target=/root/.cache/zig \
    VERSION_VALUE="${VERSION}" && \
    if [ -z "${VERSION_VALUE}" ]; then VERSION_VALUE="$(./scripts/resolve-version.sh)"; fi && \
    DATE_VALUE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    case "${TARGETARCH}" in amd64) ZIG_TARGET=x86_64 ;; arm64) ZIG_TARGET=aarch64 ;; *) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; esac && \
    CGO_ENABLED=1 \
    CC="/opt/zig/zig cc -target ${ZIG_TARGET}-linux-musl" \
    CXX="/opt/zig/zig c++ -target ${ZIG_TARGET}-linux-musl" \
    CGO_CXXFLAGS="-I/opt/spidermonkey/include" \
    CGO_LDFLAGS="-L/opt/spidermonkey/lib" \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -tags embed \
    -ldflags="-s -w -linkmode external -extldflags '-static -Wl,--strip-all' -X main.Version=${VERSION_VALUE} -X main.Commit=${COMMIT} -X main.Date=${DATE_VALUE} -X main.BuildType=release" \
    -trimpath \
    -o /app/sub2api \
    ./cmd/server

# -----------------------------------------------------------------------------
# Stage 3: PostgreSQL Client (version-matched with docker-compose)
# -----------------------------------------------------------------------------
FROM ${POSTGRES_IMAGE} AS pg-client

# -----------------------------------------------------------------------------
# Stage 4: Final Runtime Image
# -----------------------------------------------------------------------------
FROM ${ALPINE_IMAGE} AS standard

# Labels
LABEL maintainer="Wei-Shaw <github.com/Wei-Shaw>"
LABEL description="Sub2API - AI API Gateway Platform"
LABEL org.opencontainers.image.source="https://github.com/Wei-Shaw/sub2api"

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    su-exec \
    libpq \
    zstd-libs \
    lz4-libs \
    krb5-libs \
    libldap \
    libedit \
    && rm -rf /var/cache/apk/*

# Copy pg_dump and psql from the same postgres image used in docker-compose
# This ensures version consistency between backup tools and the database server
COPY --from=pg-client /usr/local/bin/pg_dump /usr/local/bin/pg_dump
COPY --from=pg-client /usr/local/bin/psql /usr/local/bin/psql
COPY --from=pg-client /usr/local/lib/libpq.so.5* /usr/local/lib/

# Create non-root user
RUN addgroup -g 1000 sub2api && \
    adduser -u 1000 -G sub2api -s /bin/sh -D sub2api

# Set working directory
WORKDIR /app

# Copy binary/resources with ownership to avoid extra full-layer chown copy
COPY --from=backend-builder --chown=sub2api:sub2api /app/sub2api /app/sub2api
COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources

# Create data directory
RUN mkdir -p /app/data && chown sub2api:sub2api /app/data
ENV DATA_DIR=/app/data

# Copy entrypoint script (fixes volume permissions then drops to sub2api)
COPY deploy/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

# Expose port (can be overridden by SERVER_PORT env var)
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD wget -q -T 5 -O /dev/null http://localhost:${SERVER_PORT:-8080}/health || exit 1

# Run the application (entrypoint fixes /app/data ownership then execs as sub2api)
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/sub2api"]

# Copy only the PostgreSQL client programs and library into the browser image.
FROM postgres:18-bookworm AS pg-client-glibc
RUN mkdir -p /export/bin /export/lib && \
    cp /usr/lib/postgresql/18/bin/pg_dump /usr/lib/postgresql/18/bin/psql /export/bin/ && \
    cp /usr/local/bin/gosu /export/bin/su-exec && \
    cp -L /usr/lib/*-linux-gnu/libpq.so.5 /export/lib/libpq.so.5

# Optional glibc browser runtime: docker build --target camoufox .
FROM debian:bookworm-slim AS camoufox
RUN apt-get update && apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
    ca-certificates tzdata libgtk-3-0 libasound2 libx11-xcb1 libdbus-glib-1-2 \
    curl libnss3 libgbm1 libxcomposite1 libxdamage1 libxrandr2 libxtst6 fonts-liberation \
    libzstd1 liblz4-1 libssl3 libgssapi-krb5-2 libldap-2.5-0 libreadline8 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 sub2api && useradd -u 1000 -g sub2api -m sub2api
COPY --from=pg-client-glibc /export/bin/ /usr/local/bin/
COPY --from=pg-client-glibc /export/lib/ /usr/local/lib/
RUN ldconfig
WORKDIR /app
COPY --from=backend-builder --chown=sub2api:sub2api /app/sub2api /app/sub2api
COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources
COPY deploy/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh && mkdir -p /app/data && chown sub2api:sub2api /app/data
ENV DATA_DIR=/app/data
# Keep the browser outside the data mount so a fresh persistent volume can use it.
RUN DATA_DIR=/app /app/sub2api --install-playground-browser && \
    rm -rf /app/runtime/camoufox/fonts/macos && \
    chown -R sub2api:sub2api /app/runtime
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD curl -fsS http://localhost:${SERVER_PORT:-8080}/health >/dev/null || exit 1
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/sub2api"]

# Preserve the default minimal image; select --target camoufox for Playground.
FROM standard AS default
