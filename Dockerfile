# syntax=docker/dockerfile-upstream:1.4.3

# mailezine engine image. Build context is the mailezine repository root
# (the mailez compose profile points here).
#
# The default image runs the pure-Go Pebble backend (no cgo). RocksDB
# production builds need a separate cgo stage (librocksdb) — see
# PLAN.md §10; the KV contract keeps both interchangeable.
FROM golang:1.26-alpine AS build
ENV GOPROXY=https://goproxy.cn,direct CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# MAILEZ_EDITION=ee compiles the enterprise features (TiDB KV, S3 blobs,
# rspamd client, HA, compliance archive) via the mailez_ee build tag; the
# default community build excludes internal/ee entirely.
ARG MAILEZ_EDITION=ce
ARG VERSION=dev
# Vendor verification key (base64 DER SubjectPublicKeyInfo; `license genkey`
# in the mailez repo prints a pair). Official EE release builds MUST inject
# it via docker-bake.hcl (repo secrets in release CI). Empty keeps the
# source-default dev key, which the engine refuses to enforce
# MAILEZINE_LICENSE_REQUIRED on (see internal/license.Load).
ARG MAILEZ_LICENSE_PUBKEY=""
RUN [ "$MAILEZ_EDITION" = "ee" ] && TAGS="-tags mailez_ee" || TAGS=""; \
    [ -n "$MAILEZ_LICENSE_PUBKEY" ] && LKEY="-X mailezine/internal/license.publicKeyB64=$MAILEZ_LICENSE_PUBKEY" || LKEY=""; \
    go build -trimpath $TAGS -ldflags="-s -w -X mailezine/internal/version.Version=${VERSION} $LKEY" -o /out/mailezine ./cmd/mailezine

FROM alpine:3.21

ARG VERSION=dev
LABEL version=$VERSION
ARG APK_MIRROR="mirrors.aliyun.com"

RUN set -euxo pipefail \
  ; sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories \
  ; apk add --no-cache ca-certificates tzdata wget \
  ; addgroup -S mailezine \
  ; adduser -S -D -G mailezine -u 82 mailezine \
  ; mkdir -p /data \
  ; chown mailezine:mailezine /data

COPY --from=build /out/mailezine /mailezine
RUN echo $VERSION >/version

# Health/metrics port. Mail protocols (25/1587/143/993/110/995/4190) are
# published by the engine itself on the compose host mappings — the gateway
# is HTTP/ACME only.
EXPOSE 11480/tcp
HEALTHCHECK --start-period=10s --interval=15s CMD wget -qO- http://127.0.0.1:11480/health >/dev/null || exit 1

# The engine runs unprivileged so files it writes (maildir messages, uidlist,
# KV spool) carry a single identity across the mail stack. Mounted data
# directories must be chowned to uid 82 (mailezine) by the deployer.
USER mailezine
CMD ["/mailezine"]
