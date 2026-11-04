# Run `make help` to see every target.

BINARY        ?= webhook
IMAGE         ?= ghcr.io/hayone1/external-dns-hostinger-webhook
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
PLATFORMS     ?= linux/amd64,linux/arm64
GOLANGCI_LINT ?= v2.15.0
LDFLAGS       := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the binary into ./bin.
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/webhook

.PHONY: run
run: ## Run locally. Reads settings from .env if it exists.
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/webhook

.PHONY: check-config
check-config: ## Test your API token and list the zones the webhook would manage.
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/webhook check

.PHONY: test
test: ## Run unit and integration tests with the race detector.
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and open a coverage report.
	go test -count=1 -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -1

.PHONY: lint
lint: ## Run golangci-lint (built with this module's Go version).
	GOTOOLCHAIN=$$(go env GOVERSION) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT) run ./...

.PHONY: fmt
fmt: ## Format the code.
	gofmt -s -w .

.PHONY: vuln
vuln: ## Look for known vulnerabilities in dependencies.
	GOTOOLCHAIN=$$(go env GOVERSION) go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum.
	go mod tidy

.PHONY: docker-build
docker-build: ## Build the image for your machine's platform.
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

.PHONY: docker-buildx
docker-buildx: ## Build the multi-arch image (no push).
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

.PHONY: e2e-images
e2e-images: ## Build the webhook and fake Hostinger API images for the end-to-end test.
	docker build --build-arg VERSION=e2e -t external-dns-hostinger-webhook:e2e .
	docker build -f test/e2e/fakehostinger/Dockerfile -t fake-hostinger:e2e .

.PHONY: e2e
e2e: ## Run the end-to-end test (E2E_MODE=fake by default, or real). See docs/development.md.
	./test/e2e/run.sh

.PHONY: e2e-k3s
e2e-k3s: ## Run the end-to-end test on a remote k3s server (set K3S_HOST in .env). Started by hand, never in CI.
	@set -a; [ -f .env ] && . ./.env; set +a; ./test/e2e/k3s.sh

.PHONY: clean
clean: ## Remove build output.
	rm -rf bin dist cover.out
