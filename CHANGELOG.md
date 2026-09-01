# Changelog

## 0.3.0 — 2026-09-02

### Added
- `Reverse` and `POST /v1/transfers/{reference}/reverse`: the mirror transfer,
  linked to the original through `reverses_transfer_id`, once per original.
  The link is hashed into the chain; earlier digests are unaffected.
- `sumzero migrate`: the schema ships inside the binary and applies
  idempotently, recorded in `sumzero_migrations`.
- Bearer authentication for the API (`--token` / `SUMZERO_API_TOKEN`).
  Without it `serve` warns at start and `GET /v1/verify` returns 403.
- golangci-lint in CI.

### Changed
- Idempotency keys are scoped to method and path.
- `httpapi.New` takes a `Config` instead of positional arguments.

### Fixed
- `Capture` on a past-due hold that the sweep had not yet marked went through.
  It is now refused with `hold_expired` and the hold is marked on the spot.

## 0.2.0 — 2026-09-01

### Added
- Holds: `Hold`, `Capture`, `Release`, `ExpireHolds`, `Available`; the
  `/v1/holds` endpoints and `GET /v1/accounts/{id}/available`.
- `allow_negative` on accounts (default `true`). When `false`, a transfer or
  hold that would push the available balance below zero is refused.
- `serve` expires past-due holds every minute.

### Changed
- Every write endpoint now requires an `Idempotency-Key`, not only transfers.
- Migration `0003_holds.sql` adds the `holds` table and the account column.

### Decided
- Accounts stay single-currency (ADR-0007).

## 0.1.0 — 2026-09-01

First release. Everything below is new.

### Engine (`ledger`)
- Accounts with a fixed currency and a normal side; archive instead of delete.
- Transfers with N debit and M credit legs, validated to net zero per currency.
- Postings are immutable: `UPDATE` and `DELETE` raise at the database level.
- Each transfer carries the digest of the previous one (tamper-evident chain).
- Materialised balances plus `BalanceAsOf`, recomputed from history.
- Embedded mode: `NewTx` joins the caller's transaction.
- `Verify`: recompute every balance, re-check sum zero, re-walk the chain.
- `Reconcile`: compare an account against an external record by reference.
- Postgres-backed idempotency store for go-idempotent.

### HTTP API (`httpapi`)
- Accounts, balances (current and as-of), statements, transfers, reconcile,
  verify, liveness and readiness.
- `POST /v1/transfers` requires an `Idempotency-Key`; replays are marked with
  `Idempotent-Replay: true`.
- Money is a string on the wire; a JSON number is rejected.

### CLI (`sumzero`)
- `serve`, `verify`, `reconcile`. Exit codes are cron-friendly: 0 clean,
  1 the books disagree, 2 the check could not run.

### Known limits
- Writes are serialised by the chain lock (ADR-0003).
- Reconciliation matches on reference only (ADR-0005).
- One currency per account, by design.
