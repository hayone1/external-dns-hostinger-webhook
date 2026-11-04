#!/usr/bin/env bash
# Prints, in a stable sorted form, the parts of Hostinger's OpenAPI file that
# this webhook depends on: the DNS zone endpoints, the domain portfolio
# endpoints, and the schemas they use. Descriptions and examples are left
# out, so only real changes show up.
#
#   hack/hostinger-api-snapshot.sh > hack/hostinger-api.json   # refresh
#   hack/hostinger-api-snapshot.sh | diff hack/hostinger-api.json -   # compare
#
# The weekly "Upstream watch" workflow opens an issue when the output no
# longer matches hack/hostinger-api.json. Needs curl and jq.
set -euo pipefail

SPEC_URL=${SPEC_URL:-https://developers.hostinger.com/openapi/openapi.json}

curl -fsSL --retry 3 "$SPEC_URL" | jq -S '
  # Drop prose that changes without changing behavior.
  def strip: walk(if type == "object" then del(.description, .summary, .example, .examples, ."x-codeSamples", ."x-scalar-ignore") else . end);
  # The paths the client calls (or must never call).
  def wanted: test("^/api/dns/v1/(zones|snapshots)/\\{domain\\}") or test("^/api/domains/v1/portfolio(/\\{domain\\})?$");
  . as $spec
  | ($spec.paths | with_entries(select(.key | wanted)) | map_values(map_values(del(.tags))) | strip) as $paths
  | {
      paths: $paths,
      schemas: ($spec.components.schemas // {}
                | with_entries(select(.key | test("^DNS\\.V1\\.(Zone|Snapshot)\\.|^Domains\\.V1\\.Domain\\.")))
                | strip),
      security: ($spec.components.securitySchemes // {} | strip)
    }'
