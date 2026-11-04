#!/usr/bin/env bash
# Run the end-to-end test (run.sh) on a remote k3s server instead of a
# cluster on your own machine. The cluster work then uses the server's CPU
# and memory, and nothing runs in a cluster on your computer.
#
# Start it by hand. It is never part of CI: GitHub always runs run.sh on kind.
#
#   make e2e-k3s                      # against the fake Hostinger API
#   E2E_MODE=real make e2e-k3s        # against the real API (HOSTINGER_API_TOKEN, E2E_ZONE)
#
# How it works:
#   1. builds the images here with docker (skip with E2E_SKIP_BUILD=true)
#   2. copies them to the server with "docker save | ssh ... k3s ctr images import"
#   3. opens an SSH tunnel to the server's Kubernetes API, so port 6443 does
#      not have to be open to the internet
#   4. runs run.sh with a kubeconfig for that tunnel
#   5. on exit: closes the tunnel, deletes the kubeconfig and removes the
#      copied images from the server
#
# run.sh only creates its own namespace (extdns-e2e-<id>) and Helm release,
# and deletes both at the end. It never touches other namespaces.
#
# Environment:
#   K3S_HOST            SSH target, for example root@203.0.113.5 (required).
#                       Keep it in your .env, not in the repository.
#   K3S_TUNNEL_PORT     local port for the API tunnel (default 16443)
#   K3S_API             skip the tunnel and use this API address instead,
#                       for example https://203.0.113.5:6443
#   K3S_TLS_SERVER_NAME name from the API certificate, needed with K3S_API
#                       when the certificate does not list that address
#   E2E_SKIP_BUILD      "true" to reuse images you built before (make e2e-images)
#   everything run.sh reads (E2E_MODE, E2E_ZONE, HOSTINGER_API_TOKEN, ...)
#
# Tools here: docker, ssh, kubectl, helm, curl, jq. On the server: k3s.

set -euo pipefail

# Values from a .env file with Windows line endings end in \r.
clean() { printf '%s' "${1:-}" | tr -d '\r'; }
K3S_HOST=$(clean "${K3S_HOST:-}")
: "${K3S_HOST:?set K3S_HOST to the SSH target of the k3s server, for example root@203.0.113.5}"
PORT=$(clean "${K3S_TUNNEL_PORT:-16443}")
API=$(clean "${K3S_API:-}")
TLS_NAME=$(clean "${K3S_TLS_SERVER_NAME:-}")
export E2E_MODE=$(clean "${E2E_MODE:-fake}")
[ -n "${E2E_ZONE:-}" ] && export E2E_ZONE=$(clean "$E2E_ZONE")
[ -n "${HOSTINGER_API_TOKEN:-}" ] && export HOSTINGER_API_TOKEN=$(clean "$HOSTINGER_API_TOKEN")

for tool in docker ssh kubectl helm curl jq; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 2; }
done

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
export E2E_ID=${E2E_ID:-$(od -An -N4 -tx4 /dev/urandom | tr -dc '0-9a-f')}
# A tag per run, so two runs never share an image and cleanup removes only ours.
export E2E_IMAGE="external-dns-hostinger-webhook:e2e-${E2E_ID}"
export E2E_FAKE_IMAGE="fake-hostinger:e2e-${E2E_ID}"
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=15)
WORK=$(mktemp -d)
chmod 700 "$WORK"
export KUBECONFIG="$WORK/kubeconfig"
SOCK="$WORK/tunnel.sock"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

cleanup() {
  local code=$?
  log "k3s: cleaning up"
  "${SSH[@]}" "$K3S_HOST" "k3s ctr -n k8s.io images rm docker.io/library/${E2E_IMAGE} docker.io/library/${E2E_FAKE_IMAGE}" >/dev/null 2>&1 || true
  [ -S "$SOCK" ] && "${SSH[@]}" -S "$SOCK" -O exit "$K3S_HOST" >/dev/null 2>&1 || true
  docker image rm "$E2E_IMAGE" "$E2E_FAKE_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  exit "$code"
}
trap cleanup EXIT

log "k3s: checking ${K3S_HOST}"
"${SSH[@]}" "$K3S_HOST" 'k3s --version | head -1'

images=("$E2E_IMAGE")
[ "$E2E_MODE" = fake ] && images+=("$E2E_FAKE_IMAGE")
if [ "${E2E_SKIP_BUILD:-false}" != "true" ]; then
  log "k3s: building images"
  docker build -q --build-arg VERSION=e2e -t external-dns-hostinger-webhook:e2e "$ROOT" >/dev/null
  [ "$E2E_MODE" = fake ] && docker build -q -f "$ROOT/test/e2e/fakehostinger/Dockerfile" -t fake-hostinger:e2e "$ROOT" >/dev/null
fi
docker tag external-dns-hostinger-webhook:e2e "$E2E_IMAGE"
[ "$E2E_MODE" = fake ] && docker tag fake-hostinger:e2e "$E2E_FAKE_IMAGE"

log "k3s: copying ${images[*]} to the server"
docker save "${images[@]}" | "${SSH[@]}" "$K3S_HOST" 'k3s ctr -n k8s.io images import -' >/dev/null

log "k3s: connecting to the Kubernetes API"
"${SSH[@]}" "$K3S_HOST" 'cat /etc/rancher/k3s/k3s.yaml' >"$KUBECONFIG"
chmod 600 "$KUBECONFIG"
cluster=$(kubectl config view -o jsonpath='{.clusters[0].name}')
if [ -z "$API" ]; then
  "${SSH[@]}" -o ExitOnForwardFailure=yes -f -N -M -S "$SOCK" -L "127.0.0.1:${PORT}:127.0.0.1:6443" "$K3S_HOST"
  # The k3s certificate lists 127.0.0.1, so the tunnel needs no extra TLS name.
  kubectl config set-cluster "$cluster" --server="https://127.0.0.1:${PORT}" >/dev/null
else
  kubectl config set-cluster "$cluster" --server="$API" >/dev/null
  [ -n "$TLS_NAME" ] && kubectl config set-cluster "$cluster" --tls-server-name="$TLS_NAME" >/dev/null
fi
kubectl get --raw /readyz >/dev/null
echo "    ok: $(kubectl version -o json | jq -r .serverVersion.gitVersion)"

"$HERE/run.sh"
