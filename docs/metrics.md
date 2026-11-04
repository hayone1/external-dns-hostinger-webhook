# Metrics and health checks

The webhook runs a small HTTP server on port **8080** (change with `HEALTH_PORT`).

## Health checks

| Path | Answers 200 when | Use it for |
|---|---|---|
| `/healthz` | the process is running and not shutting down | liveness probe |
| `/readyz` | the API token worked and the webhook API is listening | readiness probe |

While the webhook cannot reach Hostinger (for example during a network
outage at startup), `/healthz` says ok but `/readyz` does not. Kubernetes
then waits instead of restarting the pod again and again.

## Prometheus metrics

`/metrics` serves Prometheus metrics. With the external-dns Helm chart, enable
`serviceMonitor` and the webhook is scraped too
(see `provider.webhook.serviceMonitor` in the chart values).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hostinger_webhook_api_requests_total` | counter | `operation`, `code` | Calls to the Hostinger API. `code="0"` means no answer (network error). |
| `hostinger_webhook_api_request_duration_seconds` | histogram | `operation` | How long those calls took. |
| `hostinger_webhook_api_retries_total` | counter | `operation`, `reason` | Retries, by HTTP status or `network`. |
| `hostinger_webhook_api_ratelimit_remaining` | gauge | | Requests left in Hostinger's current rate limit window, as last reported. Your other Hostinger tools use the same budget. |
| `hostinger_webhook_record_changes_total` | counter | `action`, `result` | Record sets created, updated and deleted, and whether it worked. |
| `hostinger_webhook_records` | gauge | | Record sets returned to external-dns on the last read. |
| `hostinger_webhook_zones` | gauge | | Zones read on the last read. |
| `hostinger_webhook_last_success_timestamp_seconds` | gauge | | Time of the last successful read or write. |
| `hostinger_webhook_auth_failures_total` | counter | | Times Hostinger refused the API token (HTTP 401). |
| `hostinger_webhook_skipped_endpoints_total` | counter | `reason` | Records or values dropped or changed because Hostinger cannot store them as asked. |
| `hostinger_webhook_build_info` | gauge | `version` | Always 1; tells you which version runs. |

The `operation` label is one of `listDomains`, `getDomain`, `getZone`,
`updateZone`, `validateZone` and `deleteRRsets`.

Reasons in `skipped_endpoints_total`:

| Reason | Meaning |
|---|---|
| `unsupported_type` | A record type the webhook does not manage (NS, PTR, ...). |
| `set_identifier` | The endpoint uses routing policies, which Hostinger does not have. |
| `invalid_name` | A name Hostinger refuses, like `*` in the middle. |
| `invalid_target` | A value that does not fit the record type. |
| `extra_cname_target` | A CNAME with more than one target; only the first is kept. |
| `ttl_clamped` | A TTL outside 60 to 86400 seconds; the nearest allowed value is used. |
| `no_zone` | No zone in the account for the name. |
| `ownership_kept` | An ownership record kept because its record could not be deleted. |
| `owner_missing` | A record not created because its ownership record could not be created. |

Standard Go and process metrics (`go_*`, `process_*`) are there too.

## Alert ideas

```yaml
# No successful sync for 15 minutes
- alert: HostingerWebhookStale
  expr: time() - hostinger_webhook_last_success_timestamp_seconds > 900
  for: 5m

# The token was refused: it may have expired
- alert: HostingerWebhookTokenRefused
  expr: increase(hostinger_webhook_auth_failures_total[15m]) > 0

# Changes are failing
- alert: HostingerWebhookChangesFailing
  expr: increase(hostinger_webhook_record_changes_total{result="error"}[15m]) > 0

# The shared rate limit is nearly used up
- alert: HostingerWebhookRateLimitLow
  expr: hostinger_webhook_api_ratelimit_remaining < 10
  for: 10m
```
