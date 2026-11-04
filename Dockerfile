# syntax=docker/dockerfile:1
#
# Multi-stage build:
#   1. compile a static Go binary on the build machine's own platform
#      (cross-compiling is much faster than emulating arm64)
#   2. copy only that binary into a distroless image with no shell,
#      no package manager and a non-root user
#
# Base images are pinned by digest so a build today and a build next month
# use the exact same bytes. Dependabot opens a PR when a new digest ships.

ARG GO_IMAGE=golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
WORKDIR /src

# Download modules first so this layer is cached until go.mod changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify

COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOFLAGS=-buildvcs=false \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/webhook ./cmd/webhook

FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="external-dns-hostinger-webhook" \
      org.opencontainers.image.description="ExternalDNS webhook provider for Hostinger DNS" \
      org.opencontainers.image.url="https://github.com/hayone1/external-dns-hostinger-webhook" \
      org.opencontainers.image.source="https://github.com/hayone1/external-dns-hostinger-webhook" \
      org.opencontainers.image.documentation="https://github.com/hayone1/external-dns-hostinger-webhook#readme" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

COPY --from=build /out/webhook /webhook

# 65532 is the "nonroot" user that distroless ships with.
USER 65532:65532
EXPOSE 8888 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/webhook", "healthcheck"]
ENTRYPOINT ["/webhook"]
