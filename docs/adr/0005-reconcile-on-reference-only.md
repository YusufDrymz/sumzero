# 5. Reconciliation matches on reference, and on nothing else

Date: 2026-09-01
Status: accepted

## Context

The plan for this project assumed the reconciliation step would reuse
[go-reconcile](https://github.com/YusufDrymz/go-reconcile). Reading its API
again, it solves a different problem: a *recovery loop* that takes pending
records, asks the provider whether each one succeeded, and applies a fix. That
is the right tool for "our webhook never arrived"; it is not a comparison of two
lists.

What a ledger needs is the comparison: here is what the bank says moved, here
is what we recorded, where do they disagree? Forcing that through a
recovery-loop interface would have produced an abstraction neither side wanted.

The second question is how to pair entries. Bank files are messy; matching on
amount and date pairs more lines, and also pairs lines that have nothing to do
with each other.

## Decision

`Reconcile` compares by reference only. An external line pairs with the ledger
transfer that carries the same reference; the pair is then either matched or an
amount mismatch. Anything unpaired is reported as missing on one side.

Output is four buckets and three totals. Every entry lands in exactly one
bucket, so the counts are auditable: matched + mismatched + missing-in-ledger
equals the external file, and matched + mismatched + missing-externally equals
the ledger window.

A duplicated reference in the external file is an error, not a merge. Summing
duplicates would turn a defective file into an "amount mismatch" and hide where
the problem actually is.

## Consequences

- An external source with no usable reference cannot be reconciled here. That
  is a constraint on the caller's integration, stated up front, not a gap to be
  papered over with heuristics.
- Fuzzy matching is not on the roadmap. If it is ever added, it will be a
  separate, clearly labelled bucket ("probable"), never folded into "matched".
- go-reconcile stays what it is. If sumzero ever grows a "poll the PSP for the
  status of unmatched entries" step, that is where it plugs in.
