---
name: add-record-type
description: Add support for a DNS record type, or change how an existing type is converted between external-dns targets and Hostinger values. Use for MX, SRV or CAA format work, or when Hostinger or external-dns adds a record type.
---

# Add or change a record type

Hostinger and external-dns both think in record sets: a name and type with a
list of values. Each external-dns target is one Hostinger `content` string.
The conversion lives in `internal/provider/mapping.go`.

## Steps

1. Check that Hostinger accepts the type (the `type` enum of the zone
   endpoints in the OpenAPI file) and probe how it stores values with the
   `probe-hostinger-api` skill. Note added dots and quotes.
2. Check that the external-dns TXT registry can name ownership records for
   the type (`supportedRecords` in `registry/mapper/mapper.go` of the
   external-dns module). If it cannot, external-dns never cleans the type up;
   say so in the docs.
3. Add the type to `supportedTypes` in `mapping.go`.
4. Update both functions so they agree:
   - `contentFor`: target string to Hostinger content (validate the input)
   - `recordTarget`: Hostinger content to target string
   `canonicalTarget` is built from the two.
5. Add cases to `TestTargets` in `mapping_test.go`.
6. Copy the storage rules into `normalizeContent` in the fake
   (`internal/hostinger/hostingertest/server.go`) and pin them in its
   `server_test.go`.
7. Add a scenario to `test/integration/loop_test.go` that creates, updates and
   deletes the type and checks that the second run plans zero changes.
8. Document the value format in `docs/how-it-works.md` and `docs/hostinger-api.md`.
9. Run `make test lint`.

## The golden rule

`recordTarget(contentFor(t))` must equal the target that `AdjustEndpoints`
hands back to external-dns, and it must equal what `Records` returns after
Hostinger stored `contentFor(t)`. If not, external-dns will "fix" the record
on every loop, forever.
