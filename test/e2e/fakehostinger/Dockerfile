# syntax=docker/dockerfile:1
# Image of the fake Hostinger API, for the end-to-end test only. Never published.
# Build from the repository root:
#   docker build -f test/e2e/fakehostinger/Dockerfile -t fake-hostinger:e2e .

ARG GO_IMAGE=golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY internal ./internal
COPY test/e2e/fakehostinger ./test/e2e/fakehostinger
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/fakehostinger ./test/e2e/fakehostinger

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/fakehostinger /fakehostinger
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/fakehostinger"]
