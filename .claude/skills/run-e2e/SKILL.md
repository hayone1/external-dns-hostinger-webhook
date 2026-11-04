---
name: run-e2e
description: Run and debug the end-to-end test of the webhook in a real Kubernetes cluster, against the fake Hostinger API (default, no account) or the real Hostinger API. Use when asked to "run e2e", test in a cluster, or check a change before a release.
---

# Run the end-to-end test

The test lives in `test/e2e/run.sh`. It installs the external-dns Helm chart
with the webhook as a sidecar, creates Services, and checks the records.

| Mode | Talks to | Needs | Used by |
|---|---|---|---|
| `E2E_MODE=fake` (default) | fake API pod in the cluster (`test/e2e/fakehostinger`) | nothing | CI, every PR |
| `E2E_MODE=real` | developers.hostinger.com and a real zone | `HOSTINGER_API_TOKEN`, `E2E_ZONE` | maintainers, locally, before a release |

| Cluster | How | When |
|---|---|---|
| kind | `make e2e-images`, `kind load ...`, `make e2e` | CI always; local default |
| remote k3s | `make e2e-k3s` (`test/e2e/k3s.sh`, needs `K3S_HOST` in `.env`) | started by hand, when the user wants the run off their machine. Never in CI. |

**Never put a Hostinger token into GitHub.** CI must stay on the fake.

## Steps on a remote k3s server

`make e2e-k3s` does everything: builds the images, copies them over SSH,
opens an SSH tunnel to the API, runs `run.sh`, and cleans up the tunnel and
the copied images. Add `E2E_MODE=real E2E_ZONE=<zone>` for the real API.
Run it from a shell that has docker (on Windows: WSL). Never touch other
namespaces on that server.

## Steps on kind

1. Build both images: `make e2e-images`
2. Make them available to the cluster:
   - kind: `kind load docker-image external-dns-hostinger-webhook:e2e fake-hostinger:e2e`
3. Run:
   - fake: `make e2e`
   - real: ask the user which zone to use if `E2E_ZONE` is not set (never
     guess). The zone must be a domain in the account that uses Hostinger's
     name servers (`ns1/ns2.dns-parking.com`), or the DNS check fails.
     Load the token with `set -a; . ./.env; set +a` (do not print it), then
     `E2E_MODE=real make e2e`.
   - `E2E_TEST_APEX=true` tests the bare domain. On by default with the fake
     (the script first deletes the fake's default apex A record). With
     `real`, only on a zone nothing else manages.
   - `E2E_KEEP=true` keeps the namespace for debugging. Clean up afterwards.

In fake mode, step 2 makes the next 3 calls fail with HTTP 429 and
`Retry-After`, and step 3 makes the next 15 deletes fail with HTTP 500
(`POST /_fake/faults`). Every record must still be gone at the end, which
proves no record is left without its ownership record. The script also
checks that records it did not create are unchanged and that no zone reset or
snapshot restore was attempted (`GET /_fake/calls`).

A real-mode run makes roughly 15 to 25 API calls per minute. All tools of
the Hostinger user share 90 per minute.

## Reading a failure

The script prints the records it saw plus the last logs of every container.

| Symptom | Likely cause |
|---|---|
| `zone ... cannot be read with this token` | Fake: the fake pod got a different `-zones` value. Real: the domain is not in the account (HTTP 403). |
| `fake Hostinger API did not answer` | The fake image is not in the cluster, or the port-forward port (`E2E_FAKE_PORT`) is taken. |
| external-dns logs `No endpoints could be generated` | Annotations use the old `external-dns.alpha.kubernetes.io/` prefix. external-dns v0.23+ reads `external-dns.kubernetes.io/`. |
| external-dns logs `nodes is forbidden` | Chart installed with `namespaced: true`. The service source needs a ClusterRole. |
| webhook logs `skipping change: no Hostinger zone found` with an apex hint | `txtPrefix` is missing or does not end in a dot. |
| `Hostinger only allows "*" as the first label` | `--txt-wildcard-replacement` is missing. |
| `must not be used with any other type on the same name` | A CNAME collides with another record, often the default `www` CNAME. |
| DNS check times out in real mode | The zone does not use Hostinger's name servers, or the name was looked up before it existed (negative cache up to 600 s). |
| `permanent diff` failure | `AdjustEndpoints` and `Records` disagree. Reproduce in `test/integration` first. |
| A real-mode failure that fake mode does not show | The fake is missing a real API quirk. Use the `probe-hostinger-api` skill and update the fake. |

## Cleanup

The script deletes its namespace (and, in real mode, every record set under
`e2e-<id>.<zone>`) on exit. If a real-mode run was killed, list leftover
record sets with the API and delete only names under `e2e-<id>`. Send the
names exactly as the API returns them (relative, lower case): DELETE with any
other spelling answers 200 and deletes nothing.
