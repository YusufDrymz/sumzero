# 6. Holds, and the guard that makes them mean something

Date: 2026-09-01
Status: accepted

## Context

v0.1 records. It never refuses a transfer for lack of money, because a journal
that argues with its bookkeeper is not a journal. That is the right default
for a system of record — and the wrong one for a wallet balance a customer can
actually spend, where "recorded a payment the funds did not cover" is the
incident.

The payments world also has a step between "we intend to take this" and "we
took it": the authorisation. Card auth, queued payout, withdrawal under review.
Money that is not yet moved but must not be spent twice.

## Decision

Two things, and they only work together.

**`allow_negative`** on the account, default `true`. Set it to `false` and the
account gets a guard: a transfer or a hold that would push its available
balance below zero is refused, atomically, in the same locked transaction that
would have written it.

**Holds.** A hold reserves an amount on one account without posting anything.
Available balance is the balance on the account's normal side minus every
active hold. A hold ends one of three ways: `Capture` posts the transfer that
actually moves the money and closes the hold in one transaction (a partial
capture releases the remainder); `Release` cancels it; expiry, applied by a
sweep, drops it. A closed hold never reopens.

The capture rule: the transfer must draw on the held account, and by no more
than the held amount. A capture that touches a different account, or takes
more than was authorised, is not a capture of that hold.

## Why the guard is per account and defaults to off

Because most accounts in a chart are not spendable balances. Revenue, fees,
clearing and suspense accounts go negative as a matter of course. Only the
ones that model a real, finite pool need the guard — and for those the guard
is not optional, so it is a property of the account, not a flag on the call.

## Consequences

- An expired hold keeps reserving until the sweep marks it expired. `serve`
  runs that sweep every minute; an embedded caller schedules `ExpireHolds`
  itself, or accepts that expiry is advisory.
- The available-balance query is one subquery over an indexed partial index of
  active holds. It runs inside the transfer's transaction on rows that are
  already locked, so it costs a little on guarded accounts and nothing on the
  rest.
- The guard checks *after* balances move and rolls back on failure, rather than
  predicting the result. Simpler, and the same code path proves the same
  invariant whether the write came from `Post` or `Capture`.
- Holds live in their own table, outside the postings and outside the hash
  chain. They are intent, not history. `verify` does not audit them.
