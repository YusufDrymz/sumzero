# 2. Append-only postings, as-of balances

Date: 2026-09-01
Status: accepted

## Context

The first question anyone asks a ledger in an incident is "what did this
account look like *before* the bad batch?" A schema that allows `UPDATE` cannot
answer it, because there is no record of what changed.

## Decision

Postings and transfers are never updated or deleted. The database enforces it
with a trigger that raises on `UPDATE`/`DELETE`, so the rule survives an ORM, a
future migration, or a tired engineer in `psql` at 2am.

A correction is a reversing transfer that references the original. Balances
as of a timestamp are computed from postings ordered by `posted_at, id` —
ledger date, not row insertion order, because backdating is legitimate and
common.

`account_balances` exists as a materialised current balance for the hot read
path, and is explicitly a cache: `sumzero verify` recomputes every balance from
the postings and reports drift rather than trusting it.

## Consequences

- Storage grows monotonically. Acceptable: postings are small and this is the
  cheapest form of an audit trail.
- Reversal is a first-class concept in the API, not an afterthought.
- Any future feature that "just fixes" a row is rejected by construction.
