# 1. Embedded first, service second

Date: 2026-09-01
Status: accepted

## Context

Formance Ledger occupies this space and is maturing: open source, Go, a DSL
(Numscript) for transaction logic, and a platform around it. TigerBeetle owns
the extreme-throughput niche. Building another standalone ledger service that
competes on features is a losing position for one person.

Both of them are systems you *operate*: a service to deploy, its own storage
concerns, its own upgrade path. That is the right trade at their scale and the
wrong one for a team that already runs Postgres and needs correct postings
inside an existing service.

## Decision

The engine is a Go package that takes a `*pgxpool.Pool` and writes to the
caller's database. The REST binary is a thin layer over the same package, not
the other way round.

Consequences of putting the library first:

- No second datastore, no cross-service transaction. A caller can post a
  transfer in the *same* database transaction as its own domain writes, which
  removes an entire class of "payment recorded, ledger missed it" bugs.
- The schema belongs to the caller. Migrations are shipped as plain SQL for
  them to run with whatever tool they already use.
- The API surface is Go types, not a DSL. Less expressive than Numscript on
  purpose: transaction logic stays in the caller's language, where it can be
  tested and debugged with the caller's tools.

## Consequences

- Multi-tenant hosting is out of scope, and so is any feature that assumes
  sumzero owns its database.
- The library API is the compatibility surface that matters most; the REST
  contract can move faster than it.
- If the embedded angle turns out not to be wanted, this project has no reason
  to exist separately and folds back into `go-ledger`. That is the honest
  failure mode and it is cheap to reach.
