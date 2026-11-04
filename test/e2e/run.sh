#!/usr/bin/env bash
# End-to-end test: real external-dns + this webhook in a real cluster.
#
# Two modes:
#
#   E2E_MODE=fake (default, used by CI)
#     Runs the fake Hostinger API (test/e2e/fakehostinger) inside the
#     cluster. Needs no Hostinger account. Also simulates a Hostinger outage
#     and the rate limit.
#
#   E2E_MODE=real
#     Talks to the real Hostinger API and a real zone. Use it locally before
#     a release. Every name lives under e2e-<id>.<zone>, so it never touches
#     your real records. The zone must use Hostinger's name servers for the
#     DNS check to pass.
#
# What it checks:
#   1. creating Services creates A, CNAME and TXT ownership records
#   2. changing the Services changes the records, even through HTTP 429s (fake mode)
#   3. external-dns stops changing things once it has caught up (no permanent diff)
#   4. deleting the Services deletes every record, even through an outage (fake mode)
#   5. records the webhook does not own are never touched
#
# Environment:
#   E2E_MODE            fake (default) or real
#   E2E_IMAGE           webhook image already in the cluster (default external-dns-hostinger-webhook:e2e)
#   E2E_FAKE_IMAGE      fake API image already in the cluster (default fake-hostinger:e2e)
#   E2E_ZONE            zone to use. Fake mode: default example.com. Real mode: required,
#                       must be a domain in your Hostinger account.
#   HOSTINGER_API_TOKEN real mode only
#   E2E_TARGET_IPS      two public IPs, comma separated (default 1.1.1.1,1.0.0.1)
#   E2E_TEST_APEX       "true" to also test the bare domain. Default: true in fake
#                       mode, false in real mode (only use it on a zone nothing else manages).
#   E2E_ID              unique run ID (default: random)
#   E2E_KEEP            "true" to keep the namespace for debugging
#   E2E_CHART_VERSION   external-dns chart version (default: hack/external-dns-chart.version)
#   E2E_FAKE_PORT       local port for the fake API (default 18080)
#   KUBECONFIG          cluster to use (the current context is used)
#
# Tools: kubectl, helm, curl, jq. In real mode dig is used when installed.

set -euo pipefail

MODE=${E2E_MODE:-fake}
case "$MODE" in
  fake)
    # Fixed token of the fake API (internal/hostinger/hostingertest). Not secret.
    HOSTINGER_API_TOKEN=test-token
    E2E_ZONE=${E2E_ZONE:-example.com}
    APEX=${E2E_TEST_APEX:-true}
    ;;
  real)
    : "${HOSTINGER_API_TOKEN:?set HOSTINGER_API_TOKEN}"
    : "${E2E_ZONE:?set E2E_ZONE to a domain in your Hostinger account}"
    APEX=${E2E_TEST_APEX:-false}
    ;;
  *) echo "E2E_MODE must be fake or real" >&2; exit 2 ;;
esac
# A token copied from a file with Windows line endings ends in \r.
HOSTINGER_API_TOKEN=$(printf '%s' "$HOSTINGER_API_TOKEN" | tr -d '\r\n ')

ZONE=$(echo "$E2E_ZONE" | tr -d '\r' | tr '[:upper:]' '[:lower:]')
ID=${E2E_ID:-$(od -An -N4 -tx4 /dev/urandom | tr -dc '0-9a-f')}
BASE="e2e-${ID}.${ZONE}"
NS="extdns-e2e-${ID}"
RELEASE="external-dns-e2e-${ID}"
OWNER="e2e-${ID}"
IMAGE=${E2E_IMAGE:-external-dns-hostinger-webhook:e2e}
FAKE_IMAGE=${E2E_FAKE_IMAGE:-fake-hostinger:e2e}
IFS=, read -r IP1 IP2 <<<"${E2E_TARGET_IPS:-1.1.1.1,1.0.0.1}"
HERE=$(cd "$(dirname "$0")" && pwd)
# hack/external-dns-chart.version is the one place the tested chart version
# lives. The weekly "Upstream watch" workflow says when a newer one is out.
CHART_VERSION=${E2E_CHART_VERSION:-$(tr -d '[:space:]' <"$HERE/../../hack/external-dns-chart.version")}
TIMEOUT=300
PF_PID=""

if [ "$MODE" = "fake" ]; then
  # The script reaches the fake through a port-forward; the webhook through the Service.
  API_URL="http://127.0.0.1:${E2E_FAKE_PORT:-18080}"
  WEBHOOK_API_URL="http://hostinger-fake.${NS}.svc.cluster.local:8080"
else
  API_URL=${HOSTINGER_API_URL:-https://developers.hostinger.com}
  WEBHOOK_API_URL=$API_URL
fi
ZONE_PATH="/api/dns/v1/zones/${ZONE}"

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
pass() { printf '\033[1;32m    ok:\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m    FAIL:\033[0m %s\n' "$*" >&2; dump; exit 1; }

for tool in kubectl helm curl jq; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 2; }
done

# -------------------------------------------------------------- Hostinger API

api() { # METHOD PATH [BODY]
  # Retry on errors: a failed read must never look like "no records". The
  # token goes through a header file, so it never shows up in "ps".
  local args=(-sSf --retry 4 --retry-delay 3 --retry-all-errors -X "$1"
              -H @"$AUTH_HEADER" -H 'Accept: application/json')
  [ $# -ge 3 ] && args+=(-H 'Content-Type: application/json' --data "$3")
  curl "${args[@]}" "$API_URL$2"
}
AUTH_HEADER=$(mktemp)
chmod 600 "$AUTH_HEADER"
printf 'Authorization: Bearer %s\n' "$HOSTINGER_API_TOKEN" >"$AUTH_HEADER"

# Prints "name type value ttl" for every value that belongs to this run.
records() {
  api GET "$ZONE_PATH" | jq -r --arg z "$ZONE" --arg b "$BASE" --arg apex "$APEX" '
    .[] | (if .name == "@" then $z else .name + "." + $z end) as $n
    | select($n == $b or ($n | endswith("." + $b))
             or ($apex == "true" and ($n == $z or ($n | test("^extdns-[a-z]+\\." + ($z | gsub("\\."; "\\.")) + "$")))))
    | . as $s | .records[] | select(.is_disabled | not)
    | "\($n) \($s.type) \(.content) \($s.ttl)"' | sort
}

# Delete filters for every RRset this run may have created. Names must be
# sent exactly as Hostinger stores them (relative, lower case), or DELETE
# silently does nothing.
leftover_filters() {
  api GET "$ZONE_PATH" | jq -c --arg z "$ZONE" --arg b "$BASE" --arg apex "$APEX" --arg owner "$OWNER" '
    [ .[] | (if .name == "@" then $z else .name + "." + $z end) as $n
      | select($n == $b or ($n | endswith("." + $b))
               or ($apex == "true" and .type == "TXT" and ([.records[].content] | any(contains("owner=" + $owner)))))
      | {name, type} ]'
}

# foreign prints every RRset that does not belong to this run, so the test
# can prove that the webhook leaves them alone.
foreign() {
  api GET "$ZONE_PATH" | jq -S --arg rel "e2e-${ID}" --arg apex "$APEX" '
    def mine: .name == $rel or (.name | endswith("." + $rel))
      or ($apex == "true" and ((.name == "@" and .type == "A") or (.name | test("^extdns-[a-z]+$"))));
    [ .[] | select(mine | not) ] | sort_by(.name, .type)'
}

# wait_until DESCRIPTION COMMAND...: retry COMMAND until it succeeds.
wait_until() {
  local what=$1; shift
  local deadline=$(( $(date +%s) + TIMEOUT ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "timed out waiting for: $what"
    sleep 5
  done
  pass "$what"
}

# These fail when the API call fails, so an outage never counts as a pass.
has()      { local r; r=$(records) || return 1; grep -qxF -- "$1" <<<"$r"; }
has_like() { local r; r=$(records) || return 1; grep -qE -- "$1" <<<"$r"; }
missing()  { local r; r=$(records) || return 1; ! grep -qE -- "$1" <<<"$r"; }
empty()    { local r; r=$(records) || return 1; [ -z "$r" ]; }

# dig_has NAME TYPE VALUE: ask Hostinger's name servers directly. Uses IPv4
# (many CI runners have no IPv6) and falls back to TCP when UDP is blocked.
# Succeeds if any server returns VALUE.
DNS_REACHABLE=unknown
dig_has() {
  local ns out proto reached=false
  for ns in ns1.dns-parking.com ns2.dns-parking.com; do
    for proto in +notcp +tcp; do
      if out=$(dig -4 "$proto" +short +norec +time=3 +tries=1 "@$ns" "$1" "$2" 2>/dev/null); then
        reached=true
        grep -qiF -- "$3" <<<"$out" && { DNS_REACHABLE=true; return 0; }
        break
      fi
    done
  done
  $reached && DNS_REACHABLE=true || DNS_REACHABLE=false
  return 1
}

# check_dns NAME TYPE VALUE: real mode only. Waits for the answer, but only
# warns when this machine cannot reach any Hostinger name server at all.
# Only use it for names that did not exist before: Hostinger's name servers
# cache answers for the record's TTL, so a changed record can show the old
# value for that long.
check_dns() {
  if [ "$MODE" != "real" ]; then
    return
  fi
  if ! command -v dig >/dev/null; then
    echo "    skip: dig is not installed, DNS check skipped"
    return
  fi
  dig_has "$@" || true
  if [ "$DNS_REACHABLE" = "false" ]; then
    echo "    skip: cannot reach any Hostinger name server from here, DNS check skipped"
    return
  fi
  wait_until "Hostinger name servers answer $1 $2 $3" dig_has "$@"
}

# ------------------------------------------------------------------- cluster

webhook_logs() { kubectl -n "$NS" logs "deploy/${RELEASE}" -c webhook --tail=-1 2>/dev/null || true; }
mutations()    { webhook_logs | grep -cE 'msg="(wrote record set|deleted record set)"' || true; }

dump() {
  echo "---- records ----"; records || true
  echo "---- pods ----"; kubectl -n "$NS" get pods -o wide || true
  echo "---- external-dns (last 60 lines) ----"; kubectl -n "$NS" logs "deploy/${RELEASE}" -c external-dns --tail=60 || true
  echo "---- webhook (last 60 lines) ----"; kubectl -n "$NS" logs "deploy/${RELEASE}" -c webhook --tail=60 || true
  if [ "$MODE" = "fake" ]; then
    echo "---- fake Hostinger API (last 30 lines) ----"; kubectl -n "$NS" logs deploy/hostinger-fake --tail=30 || true
  fi
}

cleanup() {
  local code=$?
  if [ "${E2E_KEEP:-false}" = "true" ]; then
    log "E2E_KEEP=true, leaving namespace $NS and records in place"
    rm -f "$AUTH_HEADER"
    exit "$code"
  fi
  log "cleaning up"
  helm uninstall "$RELEASE" --namespace "$NS" --wait >/dev/null 2>&1 || true
  if [ "$MODE" = "real" ]; then
    # Remove anything the test left behind under its own name.
    local filters; filters=$(leftover_filters 2>/dev/null || echo '[]')
    if [ -n "$filters" ] && [ "$filters" != "[]" ]; then
      echo "    deleting leftover record sets: $filters"
      api DELETE "$ZONE_PATH" "{\"filters\":${filters}}" >/dev/null || true
    fi
  fi
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  kubectl delete namespace "$NS" --wait=false --ignore-not-found >/dev/null 2>&1 || true
  rm -f "$AUTH_HEADER"
  exit "$code"
}
trap cleanup EXIT

# expect_stable: external-dns must not change anything for a few loops.
expect_stable() {
  local before after
  sleep 15 # let in-flight changes finish
  before=$(mutations)
  sleep 40 # four 10s loops
  after=$(mutations)
  [ "$before" = "$after" ] || fail "external-dns kept changing records ($before -> $after changes): permanent diff"
  pass "no changes over four loops (no permanent diff)"
}

service() { # NAME HOSTNAME TARGETS [TTL]
  kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $1
  annotations:
    external-dns.kubernetes.io/hostname: "$2"
    external-dns.kubernetes.io/target: "$3"
    external-dns.kubernetes.io/ttl: "${4:-300}"
spec:
  type: ClusterIP
  ports: [{ port: 80, targetPort: 80 }]
EOF
}

external_name() { # NAME HOSTNAME TARGET
  kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $1
  annotations:
    external-dns.kubernetes.io/hostname: "$2"
spec:
  type: ExternalName
  externalName: "$3"
EOF
}

start_fake() {
  kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hostinger-fake
spec:
  replicas: 1
  selector: { matchLabels: { app: hostinger-fake } }
  template:
    metadata:
      labels: { app: hostinger-fake }
    spec:
      containers:
        - name: fake
          image: "${FAKE_IMAGE}"
          imagePullPolicy: IfNotPresent
          args: ["-zones", "${ZONE}"]
          ports: [{ containerPort: 8080 }]
          readinessProbe:
            httpGet: { path: /healthz, port: 8080 }
          securityContext:
            runAsNonRoot: true
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities: { drop: ["ALL"] }
---
apiVersion: v1
kind: Service
metadata:
  name: hostinger-fake
spec:
  selector: { app: hostinger-fake }
  ports: [{ port: 8080, targetPort: 8080 }]
EOF
  kubectl -n "$NS" rollout status deploy/hostinger-fake --timeout=2m >/dev/null
  kubectl -n "$NS" port-forward svc/hostinger-fake "${E2E_FAKE_PORT:-18080}:8080" >/dev/null 2>&1 &
  PF_PID=$!
  local i
  for i in $(seq 1 30); do
    curl -sf "$API_URL/healthz" >/dev/null && { pass "fake Hostinger API is running"; return; }
    sleep 1
  done
  fail "fake Hostinger API did not answer on $API_URL"
}

# fake_fault JSON: add a fault to the fake API (see hostingertest.Fault).
fake_fault() {
  curl -sSf -X POST "$API_URL/_fake/faults" -H 'Content-Type: application/json' --data "$1" >/dev/null
}

# ---------------------------------------------------------------------- run

log "run ${ID} (${MODE} mode): zone ${ZONE}, names under ${BASE}, image ${IMAGE}"
kubectl create namespace "$NS" >/dev/null
if [ "$MODE" = "fake" ]; then
  start_fake
fi
api GET "$ZONE_PATH" >/dev/null || { echo "zone ${ZONE} cannot be read with this token" >&2; exit 2; }
if [ "$MODE" = "fake" ] && [ "$APEX" = "true" ]; then
  # A fresh Hostinger zone points its apex to a parking address. external-dns
  # does not own that record and would not replace it, so remove it first,
  # like a user would in hPanel.
  api DELETE "$ZONE_PATH" '{"filters":[{"name":"@","type":"A"}]}' >/dev/null
fi
[ -z "$(records)" ] || { echo "records already exist under ${BASE}" >&2; exit 2; }
# Everything else in the zone must look the same at the end.
FOREIGN_BEFORE=$(foreign)

log "installing external-dns ${CHART_VERSION} with the webhook"
jq -n --arg ns "$NS" --rawfile token "$AUTH_HEADER" '{
  apiVersion: "v1", kind: "Secret", type: "Opaque",
  metadata: { name: "hostinger-credentials", namespace: $ns },
  stringData: { "api-token": ($token | sub("^Authorization: Bearer "; "") | rtrimstr("\n")) }
}' | kubectl apply -f - >/dev/null

helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/ --force-update >/dev/null
filter=$BASE
[ "$APEX" = "true" ] && filter=$ZONE
overrides=$(mktemp)
cat >"$overrides" <<EOF
provider:
  webhook:
    image:
      repository: "${IMAGE%:*}"
      tag: "${IMAGE##*:}"
    env:
      - name: HOSTINGER_API_TOKEN
        valueFrom: { secretKeyRef: { name: hostinger-credentials, key: api-token } }
      - { name: HOSTINGER_API_URL, value: "${WEBHOOK_API_URL}" }
      - { name: HOSTINGER_DEFAULT_TTL, value: "300" }
      - { name: LOG_LEVEL, value: debug }
txtOwnerId: "${OWNER}"
domainFilters: ["${filter}"]
extraArgs:
  - --txt-wildcard-replacement=wildcard
  - --webhook-provider-read-timeout=30s
  - --webhook-provider-write-timeout=60s
  - --namespace=${NS}
EOF
helm upgrade --install "$RELEASE" external-dns/external-dns \
  --namespace "$NS" --version "$CHART_VERSION" --wait --timeout 5m \
  -f "$HERE/values.yaml" -f "$overrides" >/dev/null
rm -f "$overrides"
pass "external-dns is running"

log "step 1: create records"
service web "www.${BASE}" "$IP1"
service wild "*.wild.${BASE}" "$IP1"
external_name alias "alias.${BASE}" "www.${BASE}"
[ "$APEX" = "true" ] && service apex "${ZONE}" "$IP1"

wait_until "A www.${BASE} -> ${IP1}"            has "www.${BASE} A ${IP1} 300"
wait_until "A *.wild.${BASE} -> ${IP1}"         has "*.wild.${BASE} A ${IP1} 300"
wait_until "CNAME alias.${BASE} -> www.${BASE}" has_like "^alias\.${BASE//./\\.} CNAME www\.${BASE//./\\.}\.? "
wait_until "ownership TXT for www"              has_like "^extdns-a\.www\.${BASE//./\\.} TXT .*external-dns/owner=${OWNER}"
wait_until "ownership TXT for the wildcard"     has_like "^extdns-a\.wildcard\.wild\.${BASE//./\\.} TXT .*owner=${OWNER}"
if [ "$APEX" = "true" ]; then
  wait_until "A on the zone apex ${ZONE}"       has "${ZONE} A ${IP1} 300"
  wait_until "ownership TXT for the apex"       has_like "^extdns-a\.${ZONE//./\\.} TXT .*owner=${OWNER}"
fi
check_dns "www.${BASE}" A "$IP1"
check_dns "alias.${BASE}" CNAME "www.${BASE}"
expect_stable

log "step 2: update records"
if [ "$MODE" = "fake" ]; then
  # Hostinger allows 90 requests per minute. The webhook must wait as told
  # by Retry-After and carry on.
  fake_fault '{"status":429,"count":3,"header":{"Retry-After":["2"]}}'
  pass "simulating the rate limit: the next 3 calls get HTTP 429"
fi
service web "www.${BASE}" "${IP1},${IP2}" 120
external_name alias "alias.${BASE}" "wild.${BASE}"
wait_until "A www.${BASE} has ${IP1} with TTL 120" has "www.${BASE} A ${IP1} 120"
wait_until "A www.${BASE} has ${IP2} with TTL 120" has "www.${BASE} A ${IP2} 120"
wait_until "CNAME alias.${BASE} -> wild.${BASE}"   has_like "^alias\.${BASE//./\\.} CNAME wild\.${BASE//./\\.}\.? "
wait_until "old CNAME target is gone"              missing "^alias\.${BASE//./\\.} CNAME www\."
expect_stable

log "step 3: delete records"
if [ "$MODE" = "fake" ]; then
  # Enough failures to outlast the webhook's own retries, so external-dns
  # has to try again on a later loop.
  fake_fault '{"method":"DELETE","status":500,"count":15}'
  pass "simulating a Hostinger outage: the next 15 deletes fail with HTTP 500"
fi
kubectl -n "$NS" delete service web wild alias --wait >/dev/null
[ "$APEX" = "true" ] && kubectl -n "$NS" delete service apex --wait >/dev/null
wait_until "every record of this run is gone" empty

FOREIGN_AFTER=$(foreign)
[ "$FOREIGN_BEFORE" = "$FOREIGN_AFTER" ] || fail "records the webhook does not own were changed"
pass "records the webhook does not own are unchanged"

if [ "$MODE" = "fake" ]; then
  forbidden=$(curl -sSf "$API_URL/_fake/calls" | jq '.forbidden // 0')
  [ "$forbidden" = "0" ] || fail "the webhook called zone reset or snapshot restore ($forbidden times)"
  pass "no zone reset or snapshot restore was attempted"
fi

# Errors caused by Hostinger itself (HTTP 5xx, 429) are expected now and then
# and are retried. Any other error means a bug.
transient='HTTP (5[0-9][0-9]|429)|ownership record .* could not be created'
all_errors=$(webhook_logs | grep 'level=ERROR' || true)
real_errors=$(grep -vE "$transient" <<<"$all_errors" | sed '/^$/d' || true)
if [ -n "$real_errors" ]; then
  echo "$real_errors" | tail -20
  fail "webhook logged $(wc -l <<<"$real_errors") unexpected errors"
fi
n_transient=$(sed '/^$/d' <<<"$all_errors" | wc -l)
if [ "$n_transient" -gt 0 ]; then
  pass "webhook logged no unexpected errors ($n_transient retried Hostinger API errors)"
else
  pass "webhook logged no errors"
fi

log "end-to-end test passed"
