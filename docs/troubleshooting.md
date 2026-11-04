# Troubleshooting

First, look at both containers' logs:

```sh
kubectl -n external-dns logs deploy/external-dns -c webhook
kubectl -n external-dns logs deploy/external-dns -c external-dns
```

For more detail, set `LOG_LEVEL=debug` on the webhook (and `logLevel: debug`
in the Helm values for external-dns).

You can also test your token on its own:

```sh
docker run --rm -e HOSTINGER_API_TOKEN -e DOMAIN_FILTER=example.com \
  ghcr.io/hayone1/external-dns-hostinger-webhook:0.1.0 check
```

It prints `token ok`, the list of zones, and a warning for every domain that
does not use Hostinger's name servers.

## The webhook will not start

**`invalid configuration: HOSTINGER_API_TOKEN is required`**
The Secret is missing or a key name is wrong. Compare the keys in your Secret
(`kubectl -n external-dns get secret hostinger-credentials -o yaml`) with the
`secretKeyRef` key in your Helm values.

**`Hostinger refused the API token`**
The token is wrong, was deleted, or has expired. Create a new one in hPanel
(Profile > API) and update the Secret:

```sh
kubectl -n external-dns create secret generic hostinger-credentials \
  --from-literal=api-token='new-token' --dry-run=client -o yaml | kubectl apply -f -
kubectl -n external-dns rollout restart deploy/external-dns
```

**`cannot reach Hostinger yet, retrying`**
The pod cannot reach `developers.hostinger.com`. Check network policies,
proxies and DNS inside the cluster. The webhook keeps retrying and stays
"not ready" until it gets through.

## No records are created

**external-dns logs `No endpoints could be generated from 'service/...'`**
external-dns v0.23 and newer only read annotations that start with
`external-dns.kubernetes.io/`. Change `external-dns.alpha.kubernetes.io/hostname`
to `external-dns.kubernetes.io/hostname`, or start external-dns with
`--enable-legacy-annotation-prefix`.

**Webhook logs `skipping change: no Hostinger zone found for this name`**
No domain in your Hostinger account matches that name. Fix the hostname, or
add the domain to your account. If the domain is registered somewhere else
but uses Hostinger DNS, put its name in `DOMAIN_FILTER`. If the log also has
a `hint` about the zone apex, see the next item.

**`skipping zone: Hostinger says this account does not own it`**
A name in `DOMAIN_FILTER` is not a domain in this account. Put the domain
itself (`example.com`) there, not only a sub-domain, or use the token of the
account that owns the domain.

**Records for the bare domain (`example.com`) are created but never deleted**
Your `txtPrefix` is missing or does not end in a dot. Without it, the
ownership record for `example.com` would be `a-example.com`, which is outside
the zone, so external-dns never knows it owns the record. Set
`txtPrefix: "extdns-%{record_type}."` and delete the stray record by hand once.

**`Hostinger only allows "*" as the first label of a name`**
Add `--txt-wildcard-replacement=wildcard` to external-dns's arguments.

**`must not be used with any other type on the same name`**
A CNAME cannot share its name with any other record. Most often this is the
`www` CNAME that Hostinger adds to every new zone, and you asked for an A
record on `www`. Delete the CNAME in hPanel if you want external-dns to
manage `www`.

**Nothing happens for `example.com` or `www.example.com`**
A new Hostinger zone already has an A record on the bare domain (a parking
page) and a `www` CNAME. external-dns did not create them, so it will not
replace them. Delete them in hPanel, and external-dns creates its own on the
next loop.

**`a CNAME with another target already exists and external-dns does not own it`**
Someone made that CNAME by hand. The webhook will not overwrite it. Delete it
in hPanel if external-dns should manage the name.

**`has values that are disabled in hPanel`**
Someone turned a value off in hPanel. The API cannot keep that setting when
it rewrites the record, so the webhook leaves the record alone. Turn the value
on again or delete it in hPanel.

**external-dns logs `nodes is forbidden`**
You installed the chart with `namespaced: true`. The Service source needs
to read Nodes, which a namespaced Role does not allow. Use `namespaced: false`,
and add `--namespace=<name>` to `extraArgs` if you want to limit which
namespace external-dns watches.

## The record is in hPanel, but `dig` does not show it

- **Check the name servers.** Run `dig +short NS example.com`. You must see
  `ns1.dns-parking.com.` and `ns2.dns-parking.com.`. If not, Hostinger's
  records are not used. `webhook check` warns about this too.
- **Wait for the cache.** Hostinger's name servers keep an answer for as long
  as the record's TTL. A changed or deleted record can show the old answer
  for that long. A name that was looked up before it existed can stay "not
  found" for up to 10 minutes.
- **Ask Hostinger's servers directly:** `dig +short app.example.com @ns1.dns-parking.com`

## Records keep changing on every loop

That is called a permanent diff. Please
[open a bug](https://github.com/hayone1/external-dns-hostinger-webhook/issues/new?template=bug_report.yml)
with debug logs of both containers. Until it is fixed, check:

- the same name is not managed by two clusters with the same `txtOwnerId`
- the record was not also changed by hand with different values

## Changes are slow or time out

**external-dns logs `context deadline exceeded` or `failed to apply changes with code 500`
after a timeout**
A big batch takes longer than external-dns waits. Raise
`--webhook-provider-read-timeout` and `--webhook-provider-write-timeout`
(see the example values). The webhook finishes the work anyway, and the next
loop picks up the rest.

**Webhook logs `retrying Hostinger API request` with reason 429, or
`Hostinger rate limit used up, pausing`**
Your account used up its 90 requests per minute. Every tool that uses the
Hostinger API with your account shares that budget. Make external-dns loop
less often (`interval: 5m`), limit the zones with `DOMAIN_FILTER`, or pause
other tools. Many 429s in a row can get your IP address blocked for a while.

## Undoing a mistake

Hostinger saves a snapshot of the zone at every change. You can restore an
older snapshot in hPanel or with the Hostinger API. Stop external-dns first
(`kubectl -n external-dns scale deploy/external-dns --replicas=0`), or it
will make its changes again.

## Still stuck?

[Open an issue](https://github.com/hayone1/external-dns-hostinger-webhook/issues/new/choose).
Include your versions, your Helm values (without secrets) and the logs.
