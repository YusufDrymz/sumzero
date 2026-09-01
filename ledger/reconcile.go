package ledger

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ExternalEntry is one line from the outside world: a bank statement, a PSP
// settlement report, a card processor's payout file. Amount is signed from the
// account's point of view — positive means the balance went up.
type ExternalEntry struct {
	Reference string    `json:"reference"`
	Amount    Money     `json:"amount"`
	Date      time.Time `json:"date,omitzero"`
}

// Match is one reference seen on both sides.
type Match struct {
	Reference string `json:"reference"`
	Ledger    Money  `json:"ledger"`
	External  Money  `json:"external"`
}

// ReconcileReport is the outcome of comparing an account against an external
// record. Every entry lands in exactly one bucket, so the four counts add up to
// the union of both inputs.
type ReconcileReport struct {
	Account  string    `json:"account"`
	Currency string    `json:"currency"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`

	Matched           []Match         `json:"matched"`
	AmountMismatch    []Match         `json:"amount_mismatch"`
	MissingInLedger   []ExternalEntry `json:"missing_in_ledger"`  // money moved, nobody recorded it
	MissingExternally []LedgerLine    `json:"missing_externally"` // recorded, money never moved

	LedgerTotal   int64 `json:"ledger_total"`
	ExternalTotal int64 `json:"external_total"`
	Difference    int64 `json:"difference"` // ledger minus external
}

// LedgerLine is a ledger-side entry with no external counterpart.
type LedgerLine struct {
	Reference string    `json:"reference"`
	Amount    Money     `json:"amount"`
	PostedAt  time.Time `json:"posted_at"`
}

// Clean reports whether both sides agree completely.
func (r ReconcileReport) Clean() bool {
	return len(r.AmountMismatch) == 0 && len(r.MissingInLedger) == 0 && len(r.MissingExternally) == 0
}

// Reconcile compares one account's postings in [from, to] against an external
// record, matching on reference.
//
// Matching on reference and nothing else is a deliberate limit. Fuzzy matching
// on amount and date finds more pairs and also invents pairs that do not exist,
// and a reconciliation that can be wrong quietly is worse than one that says
// "unmatched" loudly. A caller whose external source has no usable reference
// should fix that upstream; the ledger will not guess for them.
func (l *Ledger) Reconcile(ctx context.Context, accountID string, from, to time.Time, external []ExternalEntry) (ReconcileReport, error) {
	acc, err := l.Account(ctx, accountID)
	if err != nil {
		return ReconcileReport{}, err
	}
	r := ReconcileReport{
		Account: accountID, Currency: acc.Currency, From: from, To: to,
		Matched: []Match{}, AmountMismatch: []Match{},
		MissingInLedger: []ExternalEntry{}, MissingExternally: []LedgerLine{},
	}

	// Ledger side: net movement per reference, on the account's normal side so
	// it compares directly with the external sign convention.
	rows, err := l.db.Query(ctx, `
		SELECT t.reference, t.posted_at,
		       sum(CASE p.direction WHEN 'debit' THEN p.amount ELSE -p.amount END)
		FROM postings p JOIN transfers t ON t.id = p.transfer_id
		WHERE p.account_id = $1 AND t.posted_at >= $2 AND t.posted_at <= $3
		GROUP BY t.id, t.reference, t.posted_at
		ORDER BY t.posted_at, t.id`, accountID, from, to)
	if err != nil {
		return r, err
	}
	defer rows.Close()

	ledgerSide := make(map[string]LedgerLine)
	for rows.Next() {
		var line LedgerLine
		var raw int64
		if err := rows.Scan(&line.Reference, &line.PostedAt, &raw); err != nil {
			return r, err
		}
		line.Amount = Money{Amount: normalise(raw, acc.Type), Currency: acc.Currency}
		ledgerSide[line.Reference] = line
		r.LedgerTotal += line.Amount.Amount
	}
	if err := rows.Err(); err != nil {
		return r, err
	}

	// External side: a reference appearing twice in the file is a defect in the
	// file, and a summed duplicate would hide it as an amount mismatch.
	seen := make(map[string]bool, len(external))
	for i, e := range external {
		if e.Reference == "" {
			return r, fmt.Errorf("%w: external entry %d has no reference", ErrMissingReference, i)
		}
		if e.Amount.Currency != acc.Currency {
			return r, fmt.Errorf("%w: external entry %s is %s, account is %s",
				ErrCurrencyMismatch, e.Reference, e.Amount.Currency, acc.Currency)
		}
		if seen[e.Reference] {
			return r, fmt.Errorf("%w: reference %s appears twice in the external record",
				ErrDuplicateReference, e.Reference)
		}
		seen[e.Reference] = true
		r.ExternalTotal += e.Amount.Amount

		line, ok := ledgerSide[e.Reference]
		switch {
		case !ok:
			r.MissingInLedger = append(r.MissingInLedger, e)
		case line.Amount.Amount != e.Amount.Amount:
			r.AmountMismatch = append(r.AmountMismatch, Match{Reference: e.Reference, Ledger: line.Amount, External: e.Amount})
		default:
			r.Matched = append(r.Matched, Match{Reference: e.Reference, Ledger: line.Amount, External: e.Amount})
		}
		delete(ledgerSide, e.Reference)
	}

	for _, line := range ledgerSide {
		r.MissingExternally = append(r.MissingExternally, line)
	}
	sort.Slice(r.MissingExternally, func(i, j int) bool {
		a, b := r.MissingExternally[i], r.MissingExternally[j]
		if !a.PostedAt.Equal(b.PostedAt) {
			return a.PostedAt.Before(b.PostedAt)
		}
		return a.Reference < b.Reference
	})

	r.Difference = r.LedgerTotal - r.ExternalTotal
	return r, nil
}
