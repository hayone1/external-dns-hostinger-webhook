# external-dns-hostinger-webhook

Make Kubernetes keep your **Hostinger DNS** records up to date, all by itself.

This image is a webhook provider for [ExternalDNS](https://github.com/kubernetes-sigs/external-dns).
It runs next to external-dns, in the same pod, and turns its requests into
calls to the [Hostinger API](https://developers.hostinger.com/). Add a
hostname to a Service or Ingress, and the record appears in your Hostinger
DNS zone. Delete it, and the record goes away.

- **Source code and docs:** https://github.com/hayone1/external-dns-hostinger-webhook
- **Install guide:** https://github.com/hayone1/external-dns-hostinger-webhook/blob/main/docs/deployment.md
- **All settings:** https://github.com/hayone1/external-dns-hostinger-webhook/blob/main/docs/configuration.md
- **Troubleshooting:** https://github.com/hayone1/external-dns-hostinger-webhook/blob/main/docs/troubleshooting.md
- **Releases and changelog:** https://github.com/hayone1/external-dns-hostinger-webhook/releases
- **Report a bug:** https://github.com/hayone1/external-dns-hostinger-webhook/issues

## Quick start with Helm

Create an API token in hPanel (Profile > API) with an expiry date. A
Hostinger token can do everything your account can do, so keep it secret.

```sh
kubectl create namespace external-dns
kubectl -n external-dns create secret generic hostinger-credentials \
  --from-literal=api-token='...'

curl -LO https://raw.githubusercontent.com/hayone1/external-dns-hostinger-webhook/main/deploy/helm/values.yaml
# edit values.yaml: set your domain, cluster name and image tag

helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
helm upgrade --install external-dns external-dns/external-dns \
  --namespace external-dns -f values.yaml
```

Your domain must use Hostinger's name servers (`ns1.dns-parking.com` and
`ns2.dns-parking.com`).

## Supported tags

- `X.Y.Z` and `vX.Y.Z` - one exact release (use this in production)
- `X.Y` - the newest patch of a minor version
- `latest` - the newest release (not recommended for production)

Platforms: `linux/amd64`, `linux/arm64`.

## About the image

- Built from source by GitHub Actions on every release
- Based on `gcr.io/distroless/static`: no shell, no package manager
- Runs as non-root user `65532`, works with a read-only root file system
- Signed with cosign (keyless), with SBOM and build provenance attached
- Scanned with Trivy

Ports: `8888` (webhook API, `localhost` only by default), `8080` (`/healthz`,
`/readyz`, `/metrics`).

The same image is also on GitHub: `ghcr.io/hayone1/external-dns-hostinger-webhook`.

## License

Apache License 2.0. Not made by or connected to Hostinger.
