# 7. An account holds one currency, and that is not changing

Date: 2026-09-01
Status: accepted

## Context

"Multi-currency accounts" was on the v0.2 list. Looking at what it would mean:
a balance per currency inside one account, holds per currency, statements
split by currency, an available-balance query that returns a map. Every read
path grows a dimension, and every consumer has to ask "which currency?"
before it can use a number.

## Decision

An account keeps exactly one currency, fixed at creation. A customer who holds
TRY and USD has two accounts — `wallet:cust-42:TRY` and `wallet:cust-42:USD`
or whatever naming the caller prefers. A transfer may still span currencies as
long as it balances within each one, so an FX movement is a four-leg transfer
across two pairs of single-currency accounts.

This is not a limitation waiting to be lifted; it is the model. Every number
the ledger returns is already tagged with its currency and needs no further
qualification.

## Consequences

- The caller owns the naming convention for per-currency sub-accounts. The
  ledger does not impose one.
- Reporting across a customer's currencies is a caller-side join over their
  accounts. That is a query, not a ledger feature.
- FX rates never enter the ledger. The four-leg transfer records what moved
  on each side; the rate is the caller's business and lives in the
  description or the caller's own tables.
