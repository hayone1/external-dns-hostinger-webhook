# How it works

## The big picture

external-dns runs a loop, once a minute with the recommended settings:

1. It asks the webhook: **"Which records exist?"** (`GET /records`)
2. It looks at your Services and Ingresses and works out which records
   **should** exist.
3. It asks the webhook to tidy that list up (`POST /adjustendpoints`).
4. It compares the two lists and makes a plan: what to create, update and delete.
5. It sends the plan to the webhook (`POST /records`), and the webhook makes
   the changes in Hostinger.

The webhook only talks to external-dns on `localhost:8888`, inside the pod.
Nothing outside the pod can reach that port.

## Record sets

Hostinger and external-dns think alike: both work with **record sets**. A
record set is one name and type with a list of values, for example
`www.example.com A [1.1.1.1, 1.0.0.1]`. Hostinger has no record IDs. It knows
a record set only by its name and type.

Hostinger has two ways to change a zone:

- **PUT** a record set: "this name and type now has exactly these values".
- **DELETE** a record set: "this name and type is gone, with all its values".

So every change from external-dns becomes one of those two:

| external-dns asks to | The webhook |
|---|---|
| create `www A 1.1.1.1` | adds `1.1.1.1` to the set. Values that are already there stay. If the set is new, it is created. |
| update `www A` from `[1.1.1.1]` to `[8.8.8.8]` | writes the set with `8.8.8.8`. Values that someone added by hand, which external-dns never knew about, stay. |
| delete `www A 1.1.1.1` | removes `1.1.1.1` from the set. If no value is left, it deletes the set. |

This means the webhook never throws away a value it did not create. If you
add a value in hPanel next to one that external-dns manages, it survives.

A CNAME is the exception: it can only have one value, and nothing else may
use its name. The webhook will not overwrite a CNAME that someone made by
hand; it reports an error instead.

## Fresh data every time

Before it changes anything, the webhook reads the affected zones again.
Hostinger has no way to lock a zone or to say "only write if nobody changed
it". Reading right before writing keeps that window short, and a lock makes
sure the webhook's own writes never overlap.

## The order of changes

The webhook works in this order:

1. delete records
2. delete their ownership TXT records (see below)
3. update records
4. create ownership TXT records
5. create records

Deleting first lets a name switch from an A record to a CNAME: the A record
is gone before the CNAME arrives, and Hostinger does not complain.

The ownership steps protect you during Hostinger outages:

- If a record cannot be deleted, its ownership record is **kept**. external-dns
  still knows it owns the record and deletes it on a later loop.
- If an ownership record cannot be created, the record it belongs to is
  **not created** yet. Otherwise external-dns would see a record it does not
  own and would never touch it again.

Without this, a short outage could leave a record behind that points to an
address you no longer use, and nothing would ever clean it up.

## Few calls

Hostinger allows 90 API requests per minute for your whole account, so the
webhook saves calls:

- Each step sends at most **one DELETE and one PUT per zone**, with all the
  record sets of that step in it. Steps with nothing to do send nothing.
- The domain list is kept for 5 minutes.
- The client waits about one second between requests, waits as long as
  Hostinger asks when it answers HTTP 429, and pauses when Hostinger says no
  requests are left in the current minute.

## When something goes wrong

- **Hostinger refuses a record set** (HTTP 422, for example a bad value):
  Hostinger refuses the whole PUT, not just the bad set. The webhook then
  splits the batch in halves and tries each half, again and again, until it
  has found the bad sets. Everything else is written. With one bad set among
  16, that takes about 9 calls instead of 16.
- **One record fails:** the webhook keeps going with the others and reports
  all errors at the end. external-dns tries the failed ones again on its
  next loop.
- **Hostinger is busy** (HTTP 429) **or has an error** (HTTP 5xx): the
  webhook waits and tries again, a few times, with growing pauses. If it
  still fails, the whole step fails and external-dns tries again next loop.
- Creating a record that already exists counts as success. So does deleting a
  record that is already gone. This makes retries safe.

## Names

Hostinger stores names relative to the zone, in lower case: `www`, `*.apps`,
and `@` for the bare domain. The webhook turns them into full names
(`www.example.com`) for external-dns, and back into relative names when it
writes. This matters most for DELETE: Hostinger only deletes a record set if
the name is spelled exactly as stored. `www.example.com` or `WWW` would
silently delete nothing.

If you have two zones where one is inside the other (for example
`example.com` and `dev.example.com`), each name goes to the zone with the
longest match. `api.dev.example.com` goes to `dev.example.com`.

## Record types and value formats

| Type | Value external-dns uses | Stored in Hostinger as |
|---|---|---|
| A | `1.2.3.4` | `1.2.3.4` |
| AAAA | `2606:4700::1111` | `2606:4700::1111` (shortest form) |
| CNAME | `target.example.net` | `target.example.net.` (with a dot at the end) |
| TXT | `"any text"` | `"any text"` (quotes are added if missing) |
| MX | `10 mail.example.com` | `10 mail.example.com.` |
| SRV | `10 5 5060 sip.example.com` | `10 5 5060 sip.example.com.` |
| CAA | `0 issue "letsencrypt.org"` | `0 issue "letsencrypt.org"` |

NS, SOA, PTR and ALIAS are not managed. Hostinger manages NS and SOA itself
and the webhook never touches them. ALIAS records you made in hPanel are left
alone.

CAA can be read and written, but external-dns v0.24 cannot track who owns a
CAA record, so it never deletes one. Manage CAA by hand.

## What AdjustEndpoints does

Before external-dns compares lists, the webhook cleans up the "should exist"
list so it looks exactly like what Hostinger will store. This is how the
webhook avoids a **permanent diff**, where external-dns would "fix" the same
record on every loop.

It:

- lower-cases names and removes trailing dots
- writes IP addresses in their shortest form (`2606:4700:0:0::1` becomes `2606:4700::1`)
- adds quotes to TXT values, like Hostinger does
- sets the default TTL when none is given, and moves a TTL outside 60 to
  86400 seconds to the nearest allowed value, with a warning
- removes duplicate values (Hostinger refuses them)
- **drops** things Hostinger would refuse, with a warning in the log:
  - values that do not fit the record type
  - names with `*` anywhere but at the start
  - record types the webhook does not manage (NS, PTR, ...)
  - endpoints with a `set-identifier` (Hostinger has no routing policies)
  - a second CNAME value for the same name

## Ownership records

external-dns uses the **TXT registry**: next to each record it creates a TXT
record that says "external-dns owns this, cluster X". It only changes or
deletes records it owns. Records you made by hand are safe.

With `txtPrefix: "extdns-%{record_type}."` the ownership record of
`www.example.com` (type A) is `extdns-a.www.example.com`, and the one for
`example.com` itself is `extdns-a.example.com`. Both are inside your zone.

## Disabled values

In hPanel you can turn off a single value without deleting it. The API shows
such values with `is_disabled: true`, but cannot write that setting. The
webhook ignores disabled values when it reads, and it will not rewrite a
record set that has one, because the rewrite would lose the setting. It logs
an error and leaves the set alone.
