---
name: release
description: Cut a new release of the webhook (tag, publish signed multi-arch images to Docker Hub and GHCR) and check the result. Use when asked to release, publish an image, or bump the version.
---

# Cut a release

Releases are automated by `.github/workflows/release.yml`. A human picks the
version and pushes a tag. Always confirm the version with the user before
pushing a tag: a pushed tag publishes images.

## Steps

1. Make sure `main` is green: `gh run list --branch main --limit 5`.
   CI only tests against the fake Hostinger API, so also run the end-to-end
   test against the real API on your machine (`E2E_MODE=real make e2e`, see
   the `run-e2e` skill). It catches changes on Hostinger's side.
2. Pick the version with [semver](https://semver.org/):
   - fixes only: bump the patch number (`v0.1.0` to `v0.1.1`)
   - new feature or setting: bump the minor number
   - breaking change (renamed setting, changed default): bump the major
     number (or the minor number while on `v0.x`)
   - pre-release: `v0.2.0-rc.1`
3. In a PR, update `CHANGELOG.md` and the image tag in
   `deploy/helm/values.yaml` and the docs. Merge it.
4. Tag the merge commit on main and push it:
   `git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`
5. Watch it:
   `gh run watch "$(gh run list --workflow release.yml --limit 1 --json databaseId -q '.[0].databaseId')"`

The workflow runs the tests and the e2e test, builds amd64 and arm64, pushes
to GHCR, copies the same digest to Docker Hub, signs with cosign, attaches an
SBOM and provenance, scans with Trivy, creates the GitHub release and (if
`DOCKERHUB_DESCRIPTION_TOKEN` is set) updates the Docker Hub page.

## Check the result

```sh
docker buildx imagetools inspect ghcr.io/hayone1/external-dns-hostinger-webhook:X.Y.Z
cosign verify ghcr.io/hayone1/external-dns-hostinger-webhook:X.Y.Z \
  --certificate-identity-regexp '^https://github.com/hayone1/external-dns-hostinger-webhook/.github/workflows/release.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/hayone1/external-dns-hostinger-webhook:X.Y.Z --repo hayone1/external-dns-hostinger-webhook
docker run --rm docker.io/hayone/external-dns-hostinger-webhook:X.Y.Z version
```

## If something fails

- Tests or e2e failed: nothing was published. Fix it on main, delete the tag
  (`git push --delete origin vX.Y.Z && git tag -d vX.Y.Z`) and tag again.
- Docker Hub copy failed: check the `DOCKER_USERNAME` and `DOCKER_PASSWORD`
  secrets of the `release` environment, then re-run the failed jobs.
  `DOCKER_PASSWORD` needs Read & Write scope. Test a new token without
  printing it: `docker login -u hayone --password-stdin`.
- "Docker Hub description" failed with `Forbidden`: `DOCKERHUB_DESCRIPTION_TOKEN`
  needs Read, Write & Delete scope. A Read & Write token cannot edit the page.
- `cosign verify` says `no signatures found`: use cosign 3 or newer.
- Never move a tag that already published images. Release a new patch instead.
