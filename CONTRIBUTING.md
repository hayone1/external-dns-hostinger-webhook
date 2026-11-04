# Contributing

Thanks for helping! Bug reports, docs fixes and code are all welcome.

## Before you start

- For a small fix (typo, clear bug), just open a pull request.
- For a bigger change (new setting, new behavior), open an issue first so we
  can agree on the idea before you spend time on it.
- Be kind. See the [code of conduct](CODE_OF_CONDUCT.md).
- Found a security problem? Do **not** open an issue. Follow [SECURITY.md](SECURITY.md).

## What you need

- [Go](https://go.dev/dl/) (the version in `go.mod`)
- `make`
- Docker, if you want to build the image
- For the end-to-end test only: a Kubernetes cluster (kind is fine), `kubectl`,
  `helm`, `jq` and `curl`. No Hostinger account is needed.

## The everyday loop

```sh
git clone https://github.com/hayone1/external-dns-hostinger-webhook.git
cd external-dns-hostinger-webhook

make test     # unit + integration tests, with the race detector
make lint     # golangci-lint
make fmt      # format the code
make build    # build ./bin/webhook
```

`make test` does **not** need a Hostinger account. It runs against a fake
Hostinger API (`internal/hostinger/hostingertest`) that copies the real API's
quirks, and it runs the real external-dns planner and TXT registry against
the webhook (`test/integration`).

## Rules for changes

1. **Add a test.** A bug fix needs a test that fails without the fix.
2. **Keep the loop stable.** If you change how records are read or written,
   the integration tests must still show that a second run plans zero changes.
3. **Learned something new about the Hostinger API?** Update the fake server
   in `internal/hostinger/hostingertest` and the notes in
   [docs/hostinger-api.md](docs/hostinger-api.md).
4. **Never log secrets.** No API tokens or `Authorization` headers
   in logs, errors or metrics.
5. **Write docs in plain words.** Short sentences. Explain any term a new
   user might not know.

## Pull requests

- Use a short title in the [Conventional Commits](https://www.conventionalcommits.org/)
  style, for example `fix: keep TXT quotes when updating`.
- Fill in the pull request template.
- CI must pass: lint, tests, vulnerability check, image build and scan,
  workflow checks, and the end-to-end test.
- A maintainer reviews and merges it.

## Running the end-to-end test

It runs real external-dns with the webhook in a kind cluster, against a fake
Hostinger API that runs as a pod in the same cluster. No account or secrets.

```sh
make e2e-images
kind create cluster
kind load docker-image external-dns-hostinger-webhook:e2e fake-hostinger:e2e
make e2e
```

CI runs the same test on every pull request. To test against the real
Hostinger API with your own account, see [docs/development.md](docs/development.md).

## Using AI coding agents

Agents such as Claude Code, Codex or Copilot are fine to use. Read
[AGENTS.md](AGENTS.md): it lists the rules agents must follow here, and the
`.claude/skills/` folder holds step-by-step guides for common tasks. You are
still responsible for every line you submit, so read and test it.

## License

By sending a contribution, you agree that it is licensed under the
[Apache License 2.0](LICENSE), like the rest of the project.
