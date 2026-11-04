# Deployment guide

This guide takes you from zero to working DNS records, step by step.

**You need:**

- a Kubernetes cluster and `kubectl` connected to it
- [Helm](https://helm.sh/docs/intro/install/) 3 or newer
- a Hostinger account with a domain in it

## Step 1: Make sure your domain uses Hostinger DNS

DNS records only work if the internet asks Hostinger's servers about your
domain. Check which servers it asks now:

```sh
dig +short NS example.com
```

You should see:

```text
ns1.dns-parking.com.
ns2.dns-parking.com.
```

Domains bought at Hostinger use these by default. If you see other names:

- **Domain bought at Hostinger:** in hPanel, open Domains, pick the domain,
  and set the name servers back to Hostinger's.
- **Domain bought somewhere else:** log in at your domain registrar (where
  you bought it), find the setting called "nameservers" or "DNS servers",
  choose "custom", and enter the two names above. Also add the domain to your
  Hostinger account so it has a DNS zone.

A change of name servers can take from a few minutes to a day to spread
around the internet.

> **Already have records somewhere else?** Copy them (mail records like MX,
> for example) into the Hostinger zone **before** you switch the name
> servers, or they stop working.

## Step 2: Create an API token

1. Log in to hPanel.
2. Open your profile, then **API**.
3. Create a new token. **Pick an expiry date**, for example one year.
4. Copy the token right away. Hostinger shows it only once.

> **Warning:** a Hostinger API token has no limits. It can do everything
> your account can do: servers, billing, domains, not only DNS. Treat it
> like your password:
>
> - store it only in a Kubernetes Secret (next step)
> - never paste it into a values file, an issue or a chat
> - replace it before it expires, and delete it in hPanel when you stop
>   using the webhook

## Step 3: Store the token in Kubernetes

```sh
kubectl create namespace external-dns

kubectl -n external-dns create secret generic hostinger-credentials \
  --from-literal=api-token='paste-your-token-here'
```

Tip: put a space before the command so your shell does not save it in its
history. Or use a tool like
[External Secrets Operator](https://external-secrets.io/) or
[Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets).

## Step 4: Install external-dns with the webhook

Download the values file. Two options:

- **Recommended:** `external-dns-values.yaml` from the
  [newest release](https://github.com/hayone1/external-dns-hostinger-webhook/releases/latest).
  It is the example below with the image pinned by digest, so you run
  exactly the image that release built.
- [deploy/helm/values.yaml](../deploy/helm/values.yaml) from the repository.

Change the lines marked `CHANGE ME`:

- `tag`: the webhook version, for example `0.1.0` (already set in the
  release's file)
- `DOMAIN_FILTER` and `domainFilters`: your domain
- `txtOwnerId`: any name for this cluster, for example `prod-cluster`. If two
  clusters write to the same zone, each needs its own name.

Then install:

```sh
helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
helm repo update
helm upgrade --install external-dns external-dns/external-dns \
  --namespace external-dns -f values.yaml
```

Check that both containers are running (`2/2`):

```sh
kubectl -n external-dns get pods
kubectl -n external-dns logs deploy/external-dns -c webhook
```

You should see `Hostinger API token accepted` and `webhook server listening`.

> **First time?** Set `DRY_RUN` to `true` in the webhook's `env` for a first
> run. The webhook then logs what it would change and asks Hostinger to check
> it, but writes nothing. Remove it when the log looks right.

## Step 5: Create a record

Pick any Service or Ingress and add a hostname:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
  annotations:
    external-dns.kubernetes.io/hostname: app.example.com
    # Optional: how long DNS servers may cache the answer, in seconds (60 to 86400)
    external-dns.kubernetes.io/ttl: "300"
spec:
  type: LoadBalancer
  ports:
    - port: 80
  selector:
    app: my-app
```

For an Ingress, external-dns uses the hosts in the Ingress rules, so no
annotation is needed.

For Gateway API routes (HTTPRoute, GRPCRoute, TLSRoute), add for example
`gateway-httproute` to `sources` in your values file. The values file has
the lines ready; the Gateway API CRDs must be installed in your cluster.

If your load balancer only has a private IP (common on a single server or
home lab), tell external-dns your public IP:

```yaml
    external-dns.kubernetes.io/target: "203.0.113.10"
```

After about a minute:

```sh
dig +short app.example.com @ns1.dns-parking.com
```

> **The bare domain and `www`.** A new Hostinger zone already has an A record
> on `example.com` (a parking page) and a `www` CNAME. external-dns will not
> replace records it did not create. To manage `example.com` or
> `www.example.com` with external-dns, delete those two records in hPanel
> first.

## Upgrading

1. Read the [release notes](https://github.com/hayone1/external-dns-hostinger-webhook/releases).
2. Change `provider.webhook.image.tag` in your values file.
3. Run the same `helm upgrade --install` command again.

## Replacing the token

Create a new token in hPanel, then:

```sh
kubectl -n external-dns create secret generic hostinger-credentials \
  --from-literal=api-token='new-token' --dry-run=client -o yaml | kubectl apply -f -
kubectl -n external-dns rollout restart deploy/external-dns
```

When the pod is ready again, delete the old token in hPanel.

## Removing

```sh
helm uninstall external-dns --namespace external-dns
```

This stops external-dns but **leaves your DNS records in place**. To remove
records first, delete the Services or Ingresses (or their hostname
annotations) and wait one loop before you uninstall. Then delete the token
in hPanel.

## Checking an image before you use it

Every image is signed. To check that it was built by this project's GitHub
workflow, install [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
version 3 or newer and run:

```sh
cosign verify ghcr.io/hayone1/external-dns-hostinger-webhook:0.1.0 \
  --certificate-identity-regexp '^https://github.com/hayone1/external-dns-hostinger-webhook/.github/workflows/release.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The same works for `docker.io/hayone/external-dns-hostinger-webhook:0.1.0`.
cosign 2.x says `no signatures found`, because it cannot read the newer
signature format. Upgrade cosign.

From v0.2.0 on, the image also has
[SLSA Build Level 3](https://slsa.dev/spec/v1.0/levels) provenance: a signed
record of where and how it was built. Check it with
[slsa-verifier](https://github.com/slsa-framework/slsa-verifier) and the
digest from the release notes:

```sh
slsa-verifier verify-image ghcr.io/hayone1/external-dns-hostinger-webhook@sha256:<digest> \
  --source-uri github.com/hayone1/external-dns-hostinger-webhook --source-tag v0.2.0
```

## Without Helm

The webhook is a normal container. Run it as a second container in the
external-dns pod, set the environment variables from
[configuration.md](configuration.md), and start external-dns with:

```text
--provider=webhook
--webhook-provider-url=http://localhost:8888
--txt-prefix=extdns-%{record_type}.
--txt-wildcard-replacement=wildcard
--webhook-provider-read-timeout=30s
--webhook-provider-write-timeout=60s
--interval=1m
```

Use `/healthz` on port 8080 for the liveness probe and `/readyz` for the
readiness probe.
