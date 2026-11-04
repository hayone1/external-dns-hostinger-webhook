# Hostinger API notes

The [Hostinger API reference](https://developers.hostinger.com/) and its
[OpenAPI file](https://developers.hostinger.com/openapi/openapi.json)
(version 2.7.0) do not tell the whole story. This page lists what the real
API did when we tried it on 2026-11-04, with a test domain and record names
that start with `probe`.

**The fake API is the contract.** Every fact here is copied in
`internal/hostinger/hostingertest` and pinned by a test in
`internal/hostinger/hostingertest/server_test.go`. If you find out that the
real API behaves differently, change this page, the fake and the test
together. The `probe-hostinger-api` skill (`.claude/skills/`) explains how to
check safely.

## Basics

| Topic | What we saw |
|---|---|
| Address | `https://developers.hostinger.com` |
| Login | `Authorization: Bearer <token>`. Tokens are made in hPanel (Profile > API) and shown once. They can have an expiry date. |
| Token rights | **No scopes.** A token can do everything the account can do. |
| Bad token | HTTP 401 `{"message":"Unauthenticated.","correlation_id":"..."}` |
| Errors | `{"message": "...", "correlation_id": "..."}`. Messages often start with a code like `[DNS:4005]`. HTTP 422 for bad fields adds `"errors": {"zone.0.name": ["..."]}`. The webhook logs the `correlation_id`; quote it when you ask Hostinger support. |
| Sandbox | None. Every call works on the live account. |

## Rate limit

- 90 requests per minute **per user**. Every token and tool of the same user
  shares that budget.
- Every answer carries `RateLimit-Policy: "api";q=90;w=60`,
  `RateLimit: "api";r=<left>;t=<seconds>` and `X-RateLimit-Limit` /
  `X-RateLimit-Remaining`.
- `r` went down by one per call and jumped back up as the window moved. In
  our tests `t` always said 60.
- 110 calls in about a minute at a steady pace did not get a 429. The
  documentation says a 429 comes with `Retry-After`, and that many 429s in a
  row can get the caller's IP address blocked for a while.

What the webhook does: it sends 1 request per second on average (burst of
5), waits until the window resets when `r` reaches 0, honors `Retry-After`,
and retries HTTP 429 and 5xx with growing pauses.

## Domains and zones

| Call | What we saw |
|---|---|
| `GET /api/domains/v1/portfolio` | Every domain of the account, including expired ones. An unclaimed free domain has `"domain": null`. There is no "list DNS zones" call, so the webhook uses this list. |
| `GET /api/domains/v1/portfolio/{domain}` | Details, including `name_servers`. A domain that is not registered at Hostinger is HTTP 404 `[Domains:2006]`. |
| `GET /api/dns/v1/zones/{domain}` for a domain that is not in the account | HTTP 403 `[DNS:4002] Customer does not own example.com domain` |
| A domain in the account with an empty zone | HTTP 200 `[]` |

**A zone in the API is not the same as live DNS.** The API happily stores
records for a domain whose name servers point elsewhere, but nobody on the
internet sees them. Hostinger's name servers are
`ns1.dns-parking.com` and `ns2.dns-parking.com`. `webhook check` warns when a
domain uses other name servers.

## Reading a zone

`GET /api/dns/v1/zones/{domain}` returns the whole zone at once (no paging)
as a list of record sets:

```json
[{"name":"www","records":[{"content":"example.com.","is_disabled":false}],"ttl":300,"type":"CNAME"},
 {"name":"@","records":[{"content":"2.57.91.91","is_disabled":false}],"ttl":50,"type":"A"}]
```

- **There are no record IDs.** A record set is known by its name and type.
- Names are **relative** to the zone and lower case. `@` is the zone itself.
- The TTL belongs to the whole record set.
- **NS and SOA are never listed**, although Hostinger's name servers serve
  them (SOA: `ns1.dns-parking.com. dns.hostinger.com. ...`, negative-answer
  TTL 600 seconds).
- A fresh zone of a new domain had two records: `@ A 2.57.91.91` (TTL 50,
  below what the API itself accepts on writes) and `www CNAME <domain>.`.
- `is_disabled` marks a value that was turned off in hPanel. It cannot be set
  through the API. The webhook ignores disabled values when it reads and
  refuses to rewrite a record set that has them (a rewrite would drop the
  flag).
- The order of record sets and values is not stable. A values came back
  sorted, MX values did not.

## Writing: `PUT /api/dns/v1/zones/{domain}`

Body: `{"overwrite": true, "zone": [{"name": "www", "type": "A", "ttl": 300, "records": [{"content": "1.1.1.1"}]}]}`

| Case | What happened |
|---|---|
| `overwrite: true` (also the default) | Each record set with the same name and type is **replaced**. Others are created. Record sets that are not named stay as they are. |
| `overwrite: false` | Values are added to the set and its TTL is changed. Adding a value that is already there is HTTP 422 `[DNS:4008]`. |
| Answer | HTTP 200 `{"message":"Request accepted"}`. The change is visible to the next GET at once, and on the name servers within about 2 seconds for a new name. |
| One bad record set in a batch | The **whole call fails** with HTTP 422 and nothing is written. |
| 100 record sets in one call | Worked, in about 1.4 seconds. |
| Each call | Hostinger saves a zone snapshot (`GET /api/dns/v1/snapshots/{domain}`). |

### Names

| Sent | Stored as |
|---|---|
| `www` | `www` |
| `www.example.com` or `www.example.com.` | `www` |
| `example.com` or `@` | `@` |
| `Probe-Case` | `probe-case` |
| `*.wild` | `*.wild` |
| `_under` | `_under` |
| `a.*.mid` | HTTP 422 `[DNS:4005] DNS record validation error : Invalid RRset name a.*.mid.example.com` |
| `""` (empty) | HTTP 422 `The zone.0.name field is required.` |

So `*` only works as the whole first label. That is why external-dns needs
`--txt-wildcard-replacement`: its ownership record for `*.wild.example.com`
would otherwise be `extdns-a.*.wild.example.com`.

### Values

| Type | Sent | Stored as |
|---|---|---|
| A | `10.0.0.5`, `192.0.2.1` | as sent: private and documentation addresses are accepted |
| A | `not-an-ip` | HTTP 422 `"A" record content must be a valid IPv4 address!` |
| AAAA | `2606:4700:4700:0:0:0:0:1111` | `2606:4700:4700::1111` (shortest form) |
| CNAME | `target.example.net` | `target.example.net.` (a dot is added) |
| CNAME | `Target.Example.net.` | `Target.Example.net.` (case is kept) |
| ALIAS | `target.example.net` | `target.example.net.` |
| MX | `10 mail.example.com` | `10 mail.example.com.` (the priority is part of the content) |
| SRV | `10 5 5060 sip.example.com` | `10 5 5060 sip.example.com.` |
| CAA | `0 iodef mailto:a@example.com` | `0 iodef "mailto:a@example.com"` (the value gets quotes) |
| TXT | `hello` | `"hello"` (quotes are added) |
| TXT | `"heritage=external-dns,..."` | as sent |
| TXT | `"part one" "part two"` | as sent |
| TXT | 300 characters without quotes | one quoted string of 300 characters (not split) |
| TXT | `"say \"hi\" a\\b"` | `"say \"hi\" a\b"` (`\\` becomes `\`) |
| TXT | `hello "quoted"` (a bare quote inside) | HTTP 422 `[DNS:4008] DNS resource record is not valid or conflicts with another resource record` |

### Rules for a record set

| Case | Answer |
|---|---|
| TTL below 60 | HTTP 422 `Resource record TTL must be at least 60 seconds!` |
| TTL above 86400 | HTTP 422 `Resource record TTL cannot exceed 86400 seconds!` |
| No TTL | stored with TTL 1800 |
| `records: []` | HTTP 422 `The zone.0.records field is required.` (to remove a set, DELETE it) |
| The same value twice | HTTP 422 `Duplicate record in RRset ... with content ...` |
| CNAME with two values | HTTP 422 `RRset ... IN CNAME has more than one record` |
| CNAME next to any other type on the same name, either way round | HTTP 422 `RRset ... IN CNAME must not be used with any other type on the same name.` |
| Lower-case type (`a`) | HTTP 422 `The selected zone.0.type is invalid.` |

## Deleting: `DELETE /api/dns/v1/zones/{domain}`

Body: `{"filters": [{"name": "www", "type": "A"}]}`. It removes whole record
sets. There is no way to remove one value; PUT the set with the values that
should stay.

| Case | Answer |
|---|---|
| A name and type that do not exist | HTTP 200 |
| Several filters, some missing | HTTP 200, the existing ones are deleted |
| **Full name** (`www.example.com`) or **upper case** (`WWW`) | **HTTP 200, but nothing is deleted.** The name must match the stored, relative, lower-case name exactly. |
| `@` | deletes the apex record set of that type |
| `filters: []` | HTTP 422 `The filters field is required.` |
| Lower-case type | HTTP 422 `The selected filters.0.type is invalid.` |

This is the most dangerous quirk: a wrong spelling fails silently. The
webhook always sends the relative, lower-case name.

## Validate: `POST /api/dns/v1/zones/{domain}/validate`

Same body as PUT. Checks the change without writing it. The webhook uses it
in `DRY_RUN` mode.

## Calls the webhook never makes

- `POST /api/dns/v1/zones/{domain}/reset` resets the zone to Hostinger's
  defaults and wipes your records.
- `POST /api/dns/v1/snapshots/{domain}/{id}/restore` replaces the whole zone.

The client has no code for either. The fake counts any attempt, and the
tests fail if there is one.

## Name servers and caching

- A new name showed up on both `ns1` and `ns2.dns-parking.com` within about
  2 seconds of the PUT.
- A **changed** value took about 5 minutes (the record's TTL of 300 seconds)
  to show everywhere, and a **deleted** one too. During that time different
  servers gave different answers. The name servers are anycast and cache each
  answer for its TTL.
- A name that was asked for before it existed can stay "not found" for up to
  600 seconds (the SOA negative TTL).

So after a change, wait at least the record's TTL before you conclude that
something is wrong.

## Concurrency

There is no ETag, version or lock on a zone. Two writers can overwrite each
other. The webhook keeps the time between reading a zone and writing it
short, holds a lock so its own writes never overlap, and never replaces
values it did not know about.
