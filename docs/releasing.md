# Releasing

This page is for maintainers.

## How a release works

Pushing a tag like `v0.2.0` on `main` starts `.github/workflows/release.yml`:

1. **Checks** that the tag points to a commit on `main`.
2. **Tests**: unit and integration tests, then the end-to-end test (kind +
   fake Hostinger API). Run the end-to-end test against the real Hostinger
   API on your own machine first (`E2E_MODE=real`, see [development.md](development.md)).
3. **Builds** the image for `linux/amd64` and `linux/arm64`.
4. **Pushes** it to GHCR (`ghcr.io/hayone1/external-dns-hostinger-webhook`)
   and copies the same image to Docker Hub (`hayone/external-dns-hostinger-webhook`)
   with these tags: `0.2.0`, `v0.2.0`, `0.2`, `latest` (only for normal
   releases), `sha-<commit>`. From `v1.0.0` on, a `1` tag is added too.
5. **Signs** both images with cosign (keyless, through GitHub's OIDC token).
6. **Attaches** an SBOM and build provenance, and a GitHub attestation.
7. **Scans** the image with Trivy and uploads the results to the Security tab.
8. **Creates** the GitHub release with notes.
9. **Updates** the Docker Hub description from [dockerhub.md](dockerhub.md),
   if the optional token is set.

If any test fails, nothing is published.

A tag with a dash, like `v0.2.0-rc.1`, makes a pre-release: no `latest`
tag and no Docker Hub description update.

## Steps

1. Make sure CI on `main` is green.
2. Run the real-mode end-to-end test locally.
3. Update `CHANGELOG.md` and the version in `deploy/helm/values.yaml` in a PR. Merge it.
4. Tag and push:

   ```sh
   git switch main && git pull
   git tag -a v0.2.0 -m "v0.2.0"
   git push origin v0.2.0
   ```

5. Watch the run under **Actions > Release**.

## Repository setup (once)

| Where | Name | What |
|---|---|---|
| Environment `release` | secret `DOCKER_USERNAME` | Docker Hub user name |
| Environment `release` | secret `DOCKER_PASSWORD` | Docker Hub personal access token with **Read & Write** scope. This is enough to push images. |
| Environment `release` | secret `DOCKERHUB_DESCRIPTION_TOKEN` (optional) | Docker Hub token with **Read, Write & Delete** scope. Only this scope may edit the repository page; a Read & Write token gets `403 insufficient scope`. Without this secret the page is not updated. |

The `release` environment only allows `v*` tags, so a pull request can never
read these secrets. **There is no Hostinger token in GitHub:** CI only uses
the fake API.

### Replacing the Docker Hub token

Do this when the token expires or leaks.

1. On Docker Hub, go to **Account settings > Personal access tokens** and
   create a token with **Read & Write** scope.
2. Save it in GitHub (the command asks you to paste it, so it never lands in
   your shell history):

   ```sh
   gh secret set DOCKER_PASSWORD --env release --repo hayone1/external-dns-hostinger-webhook
   ```

3. Delete the old token on Docker Hub.
4. If a release failed at **Copy to Docker Hub**, re-run the failed jobs:
   `gh run rerun <run-id> --failed`.

## Versioning

We use [semantic versioning](https://semver.org/):

- **patch** (`0.1.0` to `0.1.1`): bug fixes only
- **minor** (`0.1.0` to `0.2.0`): new features; on `0.x`, also breaking changes
- **major** (`1.0.0` to `2.0.0`): breaking changes after 1.0

A breaking change is anything that makes a working setup stop working after
an upgrade: a renamed environment variable, a changed default, a removed feature.
