# Development

## Setup

Install [Go](https://go.dev/dl/) (the version in `go.mod`) and `make`. Then:

```sh
make help     # list every target
make test     # all tests, with the race detector
make lint     # golangci-lint
make build    # ./bin/webhook
```

## How the code is organized

```text
cmd/webhook/                       program entry: config, servers, signals, extra commands
internal/config/                   environment variables and validation
internal/hostinger/                Hostinger API client (token, retries, rate limit, errors)
internal/hostinger/hostingertest/  fake Hostinger API for tests
internal/provider/                 the external-dns provider: Records, AdjustEndpoints, ApplyChanges
internal/server/                   HTTP servers and health checks
internal/metrics/                  Prometheus metrics
test/integration/                  real external-dns code against the webhook and the fake API
test/e2e/                          real cluster test (run.sh) and the fake Hostinger API image
deploy/                            example Helm values and Secret
hack/                              API snapshot, tested chart version, base image check
```

## Three levels of tests

1. **Unit tests** (`internal/...`). Fast, no network. The tests of the fake
   itself (`internal/hostinger/hostingertest/server_test.go`) pin it to what
   the real API did, as written in [hostinger-api.md](hostinger-api.md).
2. **Integration tests** (`test/integration`). They wire the real
   external-dns planner, TXT registry and webhook HTTP client to our webhook
   server and the fake Hostinger API, then run the reconcile loop twice. The
   second run must plan zero changes. This catches a "permanent diff" before
   it reaches a cluster.
3. **End-to-end test** (`test/e2e/run.sh`). Real external-dns Helm chart in a
   real cluster. By default it talks to the fake Hostinger API, running as a
   pod next to external-dns. It can also talk to the real Hostinger API.

`make test` runs levels 1 and 2. They need no account and no network.

## Running locally against Hostinger

```sh
cp .env.example .env        # fill in your token
make check-config           # check the token and list zones, changes nothing
DRY_RUN=true make run       # run the webhook; changes are only logged and validated
```

The webhook then listens on `localhost:8888`. You can call it like external-dns does:

```sh
curl -H 'Accept: application/external.dns.webhook+json;version=1' localhost:8888/
curl localhost:8888/records
```

## The end-to-end test

You need a cluster in your current kube context (kind is fine), `kubectl`,
`helm`, `curl` and `jq`.

### Against the fake Hostinger API (no account needed)

This is what CI runs on every pull request.

```sh
make e2e-images                            # builds both images
kind create cluster
kind load docker-image external-dns-hostinger-webhook:e2e fake-hostinger:e2e
make e2e
```

The fake API (`test/e2e/fakehostinger`) is the same code the unit tests use
(`internal/hostinger/hostingertest`), so it copies the real API's quirks. In
this mode the test also simulates trouble: three calls get HTTP 429 while
records are updated, and 15 deletes fail with HTTP 500. Every record must
still be gone at the end, and records the test did not create must be
unchanged.

### Against the real Hostinger API

Run this before a release, or after you learn something new about the API.
It needs your own Hostinger account and a domain that uses Hostinger's name
servers. Your token stays on your machine; CI never uses it.

```sh
cp .env.example .env                       # HOSTINGER_API_TOKEN and E2E_ZONE
make e2e-images
kind load docker-image external-dns-hostinger-webhook:e2e
set -a; . ./.env; set +a
E2E_MODE=real make e2e
```

Every name the test creates is under `e2e-<random>.<zone>`, and it removes
everything when it ends, even when it fails. The real mode also asks
Hostinger's name servers for the new records with `dig`.

### On a remote k3s server

GitHub always runs the test on kind. On your own machine, kind is fine too.
If you want more compute, or want to keep test clusters off your computer,
run the same test on a k3s server you control. You start it by hand; it
never runs in CI.

You need SSH access with a key (`ssh root@your-server` works without a
password prompt), and docker, `kubectl`, `helm`, `curl` and `jq` on your
machine. The server only needs k3s.

```sh
echo 'K3S_HOST=root@your-server' >> .env   # once
make e2e-k3s                               # fake Hostinger API
E2E_MODE=real E2E_ZONE=example.com make e2e-k3s   # real API, token from .env
```

`test/e2e/k3s.sh` builds the images on your machine, copies them to the
server (`docker save | ssh ... k3s ctr images import`), reaches the
Kubernetes API through an SSH tunnel (port 6443 does not need to be open),
and runs `run.sh` there. When it ends, it closes the tunnel and removes the
copied images from the server. `run.sh` only creates its own
`extdns-e2e-<id>` namespace and deletes it at the end, so other workloads on
the server are not touched.

| Variable | What it does |
|---|---|
| `K3S_HOST` | SSH target of the server (required). Keep it in `.env`. |
| `K3S_TUNNEL_PORT` | local port for the API tunnel (default `16443`) |
| `K3S_API` | use this API address instead of the tunnel |
| `K3S_TLS_SERVER_NAME` | name from the API certificate, if `K3S_API` is an address the certificate does not list |
| `E2E_SKIP_BUILD=true` | reuse images from `make e2e-images` |

### Options

| Variable | What it does |
|---|---|
| `E2E_MODE` | `fake` (default) or `real` |
| `E2E_ZONE` | zone to use. Default `example.com` with the fake; required with `real`. |
| `E2E_TEST_APEX` | also test a record on the bare domain. On by default with the fake. With `real`, only turn it on for a zone nothing else manages. |
| `E2E_KEEP=true` | keep the namespace (and records) to debug. |
| `E2E_TARGET_IPS` | the two IPs records point to (default `1.1.1.1,1.0.0.1`). |
| `E2E_CHART_VERSION` | external-dns Helm chart version. |

## Changing behavior that touches Hostinger

1. Check the real API first (see `.claude/skills/probe-hostinger-api`).
2. Write down what you found in [hostinger-api.md](hostinger-api.md).
3. Make the fake server in `internal/hostinger/hostingertest` behave the
   same, and pin it with a test in its `server_test.go`.
4. Write a test, then the code.

## Building the image

```sh
make docker-build               # your platform
make docker-buildx              # amd64 + arm64
```

The Dockerfile builds a static binary and copies it into
`gcr.io/distroless/static-debian13:nonroot`. Base images are pinned by digest.
