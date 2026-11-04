# Notes for AI coding agents

This file tells AI agents (Claude Code, Codex, Copilot, Cursor, ...) how to
work in this repository. Humans may find it useful too.

## What this project is

An [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) webhook
provider for Hostinger DNS. external-dns runs it as a sidecar and calls it
over HTTP on `localhost:8888`. The webhook turns those calls into Hostinger
API calls. Health and metrics are on `0.0.0.0:8080`.

## Layout

| Path | What is there |
|---|---|
| `cmd/webhook/main.go` | Startup, signals, `check` / `healthcheck` / `version` commands |
| `internal/config` | Environment variables, validation, `*_FILE` secrets |
| `internal/hostinger` | Small Hostinger DNS client: Bearer token, rate limit, retries, typed errors |
| `internal/hostinger/hostingertest` | In-memory fake Hostinger API that copies the real API's quirks |
| `internal/provider` | The `provider.Provider` implementation: `Records`, `AdjustEndpoints`, `ApplyChanges` |
| `internal/server` | HTTP servers; reuses the upstream external-dns webhook handlers |
| `internal/metrics` | Prometheus metrics |
| `test/integration` | Real external-dns planner + TXT registry + webhook client against the fake API |
| `test/e2e` | Real cluster test (`run.sh`); `fakehostinger/` is the fake API image it runs in the cluster |
| `deploy/` | Example Helm values and Secret |
| `docs/` | User and maintainer docs |

## Commands

```sh
make test    # go test -race ./... (no network or Hostinger account needed)
make lint    # golangci-lint v2
make fmt     # gofmt -s
make build
make e2e-images && make e2e   # needs a cluster; uses the fake Hostinger API
E2E_MODE=real make e2e        # real Hostinger; needs HOSTINGER_API_TOKEN and E2E_ZONE (local only)
make e2e-k3s                  # same test on a remote k3s server (K3S_HOST in .env); started by hand, never in CI
```

CI always runs the end-to-end test on kind. `make e2e-k3s` is for people
who want the run off their own machine.

Run `make test` and `make lint` before you say a change is done.

## Rules

1. **Never print, log or commit secrets.** `.env`, `.mcp.json`, kubeconfigs and
   anything under `.notes/` or `_debug/` are private. Do not read `.env`
   values into the conversation; pass them to commands through the
   environment instead. A Hostinger token has no scopes: it can do
   everything the account can do.
2. **Never reset a zone, restore a snapshot or delete a whole zone** from
   code. The client has no code for these calls on purpose. Never touch NS
   or SOA. **CI never uses a real Hostinger account**: GitHub workflows test
   against the fake API only. Do not add Hostinger secrets to the repo.
3. **The fake API is the contract.** Hostinger's real behavior is written
   down in `docs/hostinger-api.md`, copied in
   `internal/hostinger/hostingertest`, and pinned by its `server_test.go`.
   If you learn something new about the real API, update all three.
4. **No permanent diff.** `test/integration` runs the external-dns loop
   twice and expects zero changes the second time. Keep it that way.
   `AdjustEndpoints` must produce exactly what `Records` returns.
5. **One bad record must not block the rest.** `ApplyChanges` keeps going and
   returns all errors at the end. A refused batch is split until the bad
   record sets are found.
6. **Ownership first, ownership last.** Ownership TXT records are created
   before their records and deleted after them; a record must never exist
   without its ownership record (see `ApplyChanges`).
7. **Never destroy values you did not create.** Hostinger replaces whole
   record sets, so every write is computed from a fresh read: creates add
   values, updates keep values external-dns never knew about, deletes remove
   only the named values.
8. **Idempotent writes.** Create of an existing record and delete of a missing
   record are both success.
9. **Mind the rate limit.** 90 requests per minute per Hostinger user, shared
   with every other tool. Do not add API calls to the loop without a reason.
10. **Plain language in docs.** Write for a 9th grader: short sentences,
   explain terms, give copy-paste examples.
11. **Do not mention other domain registrars** in docs or code. Say "your
   domain registrar". Naming Hostinger is fine.
12. **Pin GitHub Actions by full commit SHA** with the version in a comment.
   Every job sets its own minimal `permissions`.

## Hostinger API facts that surprise people

- There are **no record IDs**. A record set is addressed by name and type.
  PUT with `overwrite: true` replaces the whole set.
- Names are stored relative and lower case; `@` is the apex. PUT accepts full
  names, but **DELETE only matches the stored spelling**: `www.example.com`
  or `WWW` returns HTTP 200 and deletes nothing. Always send
  `relativeName(...)`.
- `*` is only allowed as the first label. Hence `--txt-wildcard-replacement`.
- A batched PUT is all or nothing; one bad set fails it with HTTP 422.
- TTL must be 60 to 86400. `records: []` is a 422; delete the set instead.
- TXT values get quotes added. CNAME, MX and SRV targets get a trailing dot.
- A CNAME cannot share its name with any other type.
- A fresh zone has `@ A 2.57.91.91` (TTL 50) and `www CNAME <zone>.`.
- NS and SOA are never returned by GET.
- Hostinger's name servers (`ns1/ns2.dns-parking.com`) cache answers for the
  record's TTL. New names show at once; changes take up to the TTL.
- external-dns v0.24's TXT registry cannot name ownership records for CAA.

## Skills

Step-by-step guides live in `.claude/skills/`:

- `run-e2e` - run and debug the end-to-end test
- `probe-hostinger-api` - safely check how the real Hostinger API behaves
- `add-record-type` - add or change how a record type is mapped
- `release` - cut a release and check the published image
