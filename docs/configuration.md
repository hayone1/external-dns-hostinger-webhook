# Configuration

The webhook reads its settings from environment variables. Only the API
token is required.

## API token

| Variable | Required | What it is |
|---|---|---|
| `HOSTINGER_API_TOKEN` | yes | An API token from hPanel (Profile > API). |

The token can also come from a file: set `HOSTINGER_API_TOKEN_FILE=/path/to/file`
instead, for example when you mount a Secret as a volume. Spaces and line
breaks around the value are removed. Setting both is an error.

> **Warning:** Hostinger tokens have no scopes. A token can do everything
> your account can do: servers, billing, domains, not only DNS.
>
> - Give the token an **expiry date** when you create it, and put a reminder
>   in your calendar to replace it.
> - Keep it in a Kubernetes Secret. Never put it in a values file, an image
>   or Git.
> - The webhook never logs the token, not even with `LOG_LEVEL=debug`.

## Which records to manage

| Variable | Default | What it does |
|---|---|---|
| `DOMAIN_FILTER` | (all domains) | Comma-separated list of domains, like `example.com,example.org`. Only zones and records under these names are read and changed. |
| `EXCLUDE_DOMAIN_FILTER` | (none) | Comma-separated domains to leave alone, like `internal.example.com`. |
| `REGEX_DOMAIN_FILTER` | (none) | A regular expression for names to manage. Use instead of `DOMAIN_FILTER`, not together with it. |
| `REGEX_DOMAIN_EXCLUSION` | (none) | A regular expression for names to leave alone. |

You can name a sub-domain in `DOMAIN_FILTER`. `DOMAIN_FILTER=app.example.com`
works with the domain `example.com`, and only touches names that end in
`app.example.com`.

Hostinger has no "list my DNS zones" call, so the webhook uses your domain
list (the portfolio) to find zones. A domain that is **registered somewhere
else** but uses Hostinger DNS is not in that list. Put its name in
`DOMAIN_FILTER` and the webhook uses it as a zone too.

Use the same domains here as in external-dns's `--domain-filter`
(`domainFilters` in Helm). external-dns also filters, but setting it on the
webhook too saves API calls.

## Behavior

| Variable | Default | What it does |
|---|---|---|
| `DRY_RUN` | `false` | `true` logs every change and asks Hostinger to validate it, but writes nothing. Good for a first test. |
| `HOSTINGER_DEFAULT_TTL` | `3600` | TTL in seconds for records that do not set one with the `external-dns.kubernetes.io/ttl` annotation. Must be 60 to 86400. |
| `HOSTINGER_RATE_LIMIT` | `1` | Most API requests per second, on average. Hostinger allows 90 per minute (1.5 per second) for your whole account. Allowed: above 0 and up to 1.5. |
| `HOSTINGER_RATE_BURST` | `5` | How many requests may go out at once before the rate limit applies. |
| `HOSTINGER_MAX_RETRIES` | `5` | How often to retry a request that failed because Hostinger was busy (HTTP 429) or had an error (HTTP 5xx). |
| `HOSTINGER_REQUEST_TIMEOUT` | `30s` | Time limit for one API request. Use Go duration format: `10s`, `1m`. |
| `HOSTINGER_ZONE_CACHE_TTL` | `5m` | How long the domain list is kept before it is read again. `0s` reads it on every loop. A new domain shows up after at most this long. |

## Network

| Variable | Default | What it does |
|---|---|---|
| `WEBHOOK_HOST` | `localhost` | Address of the API that external-dns calls. Keep `localhost` so only the pod can reach it. |
| `WEBHOOK_PORT` | `8888` | Port of that API. |
| `HEALTH_HOST` | `0.0.0.0` | Address of the health and metrics server. |
| `HEALTH_PORT` | `8080` | Port of the health and metrics server. The Helm chart expects 8080. |

## Advanced

| Variable | Default | What it does |
|---|---|---|
| `HOSTINGER_API_URL` | `https://developers.hostinger.com` | Hostinger API address. Only change it for testing. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. `debug` logs every API call (never the token). |
| `LOG_FORMAT` | `text` | `text` or `json`. |

## Recommended external-dns settings

These are external-dns settings, not webhook settings. The example
[values file](../deploy/helm/values.yaml) already has them.

| Helm value / flag | Recommended | Why |
|---|---|---|
| `txtPrefix` / `--txt-prefix` | `extdns-%{record_type}.` | Keeps ownership records for the bare domain inside the zone. **Must end in a dot.** |
| `--txt-wildcard-replacement` | `wildcard` | Hostinger refuses `*` in the middle of a name. |
| `--webhook-provider-read-timeout` | `30s` | The webhook sends about one request per second, so large batches take time. |
| `--webhook-provider-write-timeout` | `60s` | Same reason. |
| `interval` / `--interval` | `1m` or more | Each loop costs API calls, and your account has 90 per minute for all tools. |
| `policy` / `--policy` | `sync` or `upsert-only` | `sync` deletes records you remove from Kubernetes. `upsert-only` never deletes. |
| `txtOwnerId` / `--txt-owner-id` | a unique name per cluster | Lets several clusters share one zone without fighting. |

## How many API calls?

Per external-dns loop, the webhook makes:

- one call per zone to read it, plus one call every 5 minutes for the
  domain list
- when something changed: one more read per changed zone, and one or two
  writes per step that has work (see [how it works](how-it-works.md))

With 3 zones and `interval: 1m`, that is about 3 to 4 calls per minute when
nothing changes.

## Commands

The image has a few extra commands. They read the same environment variables.

```sh
# Check the token, list the zones the webhook would manage, and warn about
# domains that do not use Hostinger's name servers
docker run --rm -e HOSTINGER_API_TOKEN ghcr.io/hayone1/external-dns-hostinger-webhook:0.1.0 check

# Print the version
docker run --rm ghcr.io/hayone1/external-dns-hostinger-webhook:0.1.0 version
```

`healthcheck` is used by the image's Docker `HEALTHCHECK` and checks the local
`/healthz` endpoint.
