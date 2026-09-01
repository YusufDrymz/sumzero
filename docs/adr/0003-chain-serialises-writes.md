# 3. The hash chain serialises writes

Date: 2026-09-01
Status: accepted

## Context

Each transfer stores the digest of the previous one. To compute a link, a writer
must read the current tail — and two writers that read the same tail produce two
transfers claiming the same predecessor. The chain would then have a fork in it
and prove nothing.

## Decision

`Post` takes a transaction-scoped advisory lock before reading the tail and
inserting. Concurrent posts queue behind it and the chain stays a straight line.

Row locks on the touched accounts are taken *before* the chain lock, and always
in id order, so two transfers over the same accounts cannot deadlock each other.

## Consequences

- Writes are serialised globally. This caps throughput at roughly what one
  Postgres transaction round-trip allows — order of a few thousand transfers a
  second, and only if the surrounding work is small.
- That ceiling is fine for the target: services recording payments and payouts
  in their own database. It is not fine for exchange-grade volume, which is what
  TigerBeetle is for, and the README should not pretend otherwise.
- If the ceiling ever binds, the way out is per-currency or per-book chains
  rather than dropping the chain. That is a schema change, so the option is kept
  open by keeping the lock key a constant in one place.
