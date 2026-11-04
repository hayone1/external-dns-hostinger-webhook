# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/).

## [Unreleased]

### Added

- `make e2e-k3s` (`test/e2e/k3s.sh`) runs the end-to-end test on a remote
  k3s server over SSH, for maintainers who want the run off their own
  machine. CI keeps using kind.
- Releases have SLSA Build Level 3 provenance and ship
  `external-dns-values.yaml` with the image pinned by digest.
- The example values file lists the Gateway API sources.
- Weekly "Upstream watch" workflow: opens an issue when Hostinger's API or
  the external-dns chart changes, and scans the newest image.
- Dependabot patch, minor and digest updates merge on their own once CI
  passes.
- CI checks that base images are pinned by digest and that the distroless
  image is signed.

### Changed

- Domains that are not active in the Hostinger account (expired, suspended,
  waiting for setup) are skipped, which saves API calls. Name one in
  `DOMAIN_FILTER` to manage it anyway.

## [0.1.0] - 2026-11-04

First release.

### Added

- ExternalDNS webhook provider for Hostinger DNS, built on external-dns v0.24.
- Create, update and delete A, AAAA, CNAME, TXT, MX, SRV and CAA records.
- Record sets are merged, never blindly replaced: values that external-dns
  did not create are kept, and a CNAME made by hand is never overwritten.
- Domain filters: include, exclude and regular expressions. Domains
  registered elsewhere but hosted on Hostinger DNS can be named in
  `DOMAIN_FILTER`.
- Dry-run mode that asks Hostinger to validate every change.
- Hostinger client with Bearer token, rate limiting (1 request per second by
  default), a pause when the shared 90-per-minute budget runs out, retries
  with backoff for 429 and 5xx, and `Retry-After` support.
- Ownership-safe ordering: ownership TXT records are created before and
  deleted after the records they own, so a Hostinger outage can never leave
  an orphaned record behind.
- Few API calls: one PUT and one DELETE per zone and step. A batch that
  Hostinger refuses is split in halves until the bad record sets are found,
  so one bad record never blocks the rest.
- Desired endpoints Hostinger would refuse (`*` in the middle of a name,
  NS/PTR, set-identifier, TTL outside 60 to 86400) are dropped or fixed with a
  warning instead of failing the whole batch.
- The API token can be read from a file (`HOSTINGER_API_TOKEN_FILE`).
- `/healthz`, `/readyz` and Prometheus `/metrics` on port 8080.
- `check` command that tests the token, lists zones and warns about domains
  that do not use Hostinger's name servers. `healthcheck` and `version`
  commands.
- Signed multi-arch images on Docker Hub and GHCR, with SBOM and provenance.

[Unreleased]: https://github.com/hayone1/external-dns-hostinger-webhook/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/hayone1/external-dns-hostinger-webhook/releases/tag/v0.1.0
