# 4. The API refuses a write without an idempotency key

Date: 2026-09-01
Status: accepted

## Context

A client that posts a transfer and then loses the connection cannot tell whether
the money moved. Its only safe options are to retry (and risk posting twice) or
to give up (and risk losing the record). Both are wrong.

The `reference` column is already unique, so the ledger itself cannot double
post — a retry gets a `duplicate_reference` conflict. But a 409 is not an
answer: the client still does not know whether *its* request was the one that
succeeded, or what the resulting transfer looks like.

## Decision

`POST /v1/transfers` requires an `Idempotency-Key` header and fails with 400
without one. The same key replays the original response, body and status
included; the same key with a different body is rejected with 422.

Keys are stored in Postgres, in the same database as the ledger, through the
`Store` interface of [go-idempotent](https://github.com/YusufDrymz/go-idempotent).
The library's own middleware passes unkeyed requests through, which is correct
for a general-purpose package and wrong for this endpoint, so the requirement is
a thin wrapper in front of it.

## Consequences

- Two layers of defence, and they answer different questions: the unique
  reference guarantees the ledger stays correct, the key guarantees the client
  gets a usable answer.
- Keys must live in the ledger's database rather than in memory. Two API
  instances with in-memory stores would each believe they were first.
- Completed keys expire (24h by default). After that a retry is treated as a new
  request and stopped by the unique reference instead — degraded, but still not
  double-posted.
- `Release` on a failed handler frees the key, so a transient error does not
  block the retry that is supposed to fix it.
