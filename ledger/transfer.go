package ledger

import (
	"fmt"
	"time"
)

// Posting is one leg of a transfer.
type Posting struct {
	Account string    `json:"account"`
	Amount  Money     `json:"amount"`
	Dir     Direction `json:"direction"`
}

// Transfer is a set of postings applied as one unit. It is written once and
// never updated: a mistake is corrected with a reversing transfer, the way a
// paper ledger is corrected. That is what makes as-of balances honest.
type Transfer struct {
	// Reference is the caller's own id for this movement (order id, payment
	// id, …). It is required, indexed, and shows up in reconciliation reports.
	Reference   string    `json:"reference"`
	Description string    `json:"description"`
	Postings    []Posting `json:"postings"`

	// PostedAt is the ledger date. Zero means "now" at write time. It may be
	// backdated, which is why as-of queries order by this and not by row id.
	PostedAt time.Time `json:"posted_at"`
}

// Debit adds a debit leg. Chainable.
func (t *Transfer) Debit(account string, m Money) *Transfer {
	t.Postings = append(t.Postings, Posting{Account: account, Amount: m, Dir: Debit})
	return t
}

// Credit adds a credit leg. Chainable.
func (t *Transfer) Credit(account string, m Money) *Transfer {
	t.Postings = append(t.Postings, Posting{Account: account, Amount: m, Dir: Credit})
	return t
}

// Net returns the debit-minus-credit total per currency. A valid transfer nets
// to zero in every currency it touches — the whole invariant in one line.
func (t *Transfer) Net() map[string]int64 {
	net := make(map[string]int64, 1)
	for _, p := range t.Postings {
		net[p.Amount.Currency] += p.Dir.signed(p.Amount.Amount)
	}
	return net
}

// Validate checks everything that can be known without touching the database:
// shape, amounts, directions, and the sum-zero invariant. Account existence and
// currency agreement need the store and are checked there.
func (t *Transfer) Validate() error {
	if t.Reference == "" {
		return ErrMissingReference
	}
	if len(t.Postings) < 2 {
		return ErrTooFewPostings
	}
	for i, p := range t.Postings {
		if p.Account == "" {
			return fmt.Errorf("%w: posting %d", ErrInvalidAccount, i)
		}
		if !p.Dir.valid() {
			return fmt.Errorf("%w: posting %d has %q", ErrInvalidDirection, i, p.Dir)
		}
		if !validCurrency(p.Amount.Currency) {
			return fmt.Errorf("%w: posting %d has %q", ErrInvalidCurrency, i, p.Amount.Currency)
		}
		// Signed amounts would let a caller express a credit as a negative
		// debit, and then two representations of the same movement exist.
		if p.Amount.Amount <= 0 {
			return fmt.Errorf("%w: posting %d", ErrNonPositiveAmount, i)
		}
	}
	for cur, net := range t.Net() {
		if net != 0 {
			return fmt.Errorf("%w: %s off by %d", ErrUnbalanced, cur, net)
		}
	}
	return nil
}
