---
name: probe-hostinger-api
description: Safely check how the real Hostinger DNS API behaves (name formats, error codes, quoting, limits, caching) and record the findings in the fake API, its tests and the docs. Use when a test against the fake passes but the real API behaves differently, or before relying on an undocumented behavior.
---

# Probe the Hostinger API

The OpenAPI file (https://developers.hostinger.com/openapi/openapi.json)
does not tell the whole story. Check the real API before you depend on a
behavior. There is no sandbox: every call hits the live account.

## Rules

- Use a zone the user named. Ask before writing to any other domain; some
  domains in the account carry live websites and mail.
- Use record names that start with `probe` so they are easy to find and remove.
- **Never call** `POST /api/dns/v1/zones/{domain}/reset` or
  `POST /api/dns/v1/snapshots/{domain}/{id}/restore`. Never write NS or SOA.
- Never print the token. Load it from `.env` into the environment.
- Mind the rate limit: 90 calls per minute for the whole user, shared with
  the user's other tools. Do not hammer the API to find the 429; repeated
  429s can get the IP address blocked.
- Remove every probe record when you are done, and show the user the final
  record list.

## Helper

```sh
set -a; . ./.env; set +a   # HOSTINGER_API_TOKEN (strip \r if .env has Windows line endings)

hapi() { # METHOD PATH [JSON]
  local a=(-sS -X "$1" -H "Authorization: Bearer $HOSTINGER_API_TOKEN"
           -H 'Accept: application/json' -w '\nHTTP %{http_code}\n')
  [ $# -ge 3 ] && a+=(-H 'Content-Type: application/json' --data "$3")
  curl "${a[@]}" "https://developers.hostinger.com$2"
}

Z=/api/dns/v1/zones/$ZONE
hapi GET "$Z"
hapi PUT "$Z" '{"overwrite":true,"zone":[{"name":"probe-a","type":"A","ttl":300,"records":[{"content":"1.1.1.1"}]}]}'
hapi POST "$Z/validate" '{"zone":[{"name":"probe-a","type":"A","ttl":30,"records":[{"content":"1.1.1.1"}]}]}'
hapi DELETE "$Z" '{"filters":[{"name":"probe-a","type":"A"}]}'
```

To see what the name servers serve: `dig -4 +short +norec @ns1.dns-parking.com probe-a.$ZONE A`.
They cache each answer for its TTL, so test changes with a fresh name.

## After probing

1. Write the finding in `docs/hostinger-api.md` with the date.
2. Copy the behavior into `internal/hostinger/hostingertest/server.go`.
3. Pin it with a test in `internal/hostinger/hostingertest/server_test.go`,
   and add or change a provider or integration test that depends on it.
4. Delete the probe record sets. Collect their names **exactly as GET
   returns them** and send one DELETE:
   `hapi DELETE "$Z" '{"filters":[{"name":"probe-a","type":"A"},{"name":"probe-b","type":"TXT"}]}'`.
   A full name (`probe-a.example.com`) or upper case deletes nothing and
   still answers 200, so check with a GET afterwards.
