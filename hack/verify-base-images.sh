#!/usr/bin/env bash
# Checks the base images of the given Dockerfiles (default: Dockerfile and
# the fake API's Dockerfile):
#
#   - every image must be pinned by digest (name@sha256:...)
#   - distroless images must carry a valid cosign signature from the
#     distroless project (keyless, Google service account)
#
# Docker Official Images such as golang:alpine have no cosign signature, so
# only their digest pin is checked. They are build-only and never shipped.
#
# Needs cosign 3 or newer.
set -euo pipefail

files=("$@")
[ ${#files[@]} -gt 0 ] || files=(Dockerfile test/e2e/fakehostinger/Dockerfile)

status=0
# Images are set as ARG defaults: ARG GO_IMAGE=golang:...@sha256:...
images=$(sed -nE 's/^ARG [A-Z_]+_IMAGE=(.+)$/\1/p' "${files[@]}" | sort -u)
[ -n "$images" ] || { echo "no base images found in ${files[*]}" >&2; exit 1; }

for image in $images; do
  if [[ "$image" != *@sha256:* ]]; then
    echo "::error::base image $image is not pinned by digest"
    status=1
    continue
  fi
  case "$image" in
    gcr.io/distroless/*)
      if cosign verify "$image" \
          --certificate-identity keyless@distroless.iam.gserviceaccount.com \
          --certificate-oidc-issuer https://accounts.google.com >/dev/null 2>&1; then
        echo "ok: $image (pinned, signature verified)"
      else
        echo "::error::cosign could not verify the signature of $image"
        status=1
      fi
      ;;
    *)
      echo "ok: $image (pinned)"
      ;;
  esac
done
exit $status
