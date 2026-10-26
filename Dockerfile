FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 \
    GOOS="${TARGETOS:-linux}" \
    GOARCH="${TARGETARCH:-amd64}" \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/opencode2api ./cmd/opencode2api

FROM oven/bun:1.4.3-alpine@sha256:629e17411f1f129dbec3af78d5af9c9f2a937435c80349437206c6b0b7422373 AS bun-runtime

FROM alpine:3.22

RUN apk add --no-cache ca-certificates su-exec tzdata libstdc++ libgcc \
    && addgroup -S opencode2api \
    && adduser -S -G opencode2api -h /var/lib/opencode2api opencode2api \
    && mkdir -p /app /var/lib/opencode2api \
    && chown -R opencode2api:opencode2api /app /var/lib/opencode2api

COPY --from=builder /out/opencode2api /usr/local/bin/opencode2api
COPY --from=bun-runtime /usr/local/bin/bun /usr/local/bin/bun
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint
COPY --chown=opencode2api:opencode2api config.example.json /app/config.example.json
COPY LICENSE /usr/share/licenses/opencode2api/LICENSE
COPY NOTICE /usr/share/licenses/opencode2api/NOTICE
COPY LICENSES/ /usr/share/licenses/opencode2api/third-party/
LABEL org.opencontainers.image.licenses="GPL-3.0-or-later"

# LISTEN_ADDRESS / WEBUI_LISTEN_ADDRESS are intentionally unset: config.json is
# authoritative. Export them to override the configured listen addresses.
ENV OPENCODE2API_BUN_BINARY=/usr/local/bin/bun \
    CONFIG_PATH=/var/lib/opencode2api/config.json \
    CONFIG_SEED_PATH= \
    LISTEN_ADDRESS= \
    WEBUI_LISTEN_ADDRESS= \
    STATE_DIR=/var/lib/opencode2api

WORKDIR /app

EXPOSE 8080 8081

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint"]
