package ledger

import (
	"errors"
	"math/rand"
	"testing"
)

func try(minor int64) Money { return Amount(minor, "TRY") }

func TestTransferValidate(t *testing.T) {
	tests := []struct {
		name string
		tx   *Transfer
		want error
	}{
		{
			name: "balanced two legs",
			tx:   (&Transfer{Reference: "ord-1"}).Debit("cash", try(10000)).Credit("revenue", try(10000)),
		},
		{
			name: "balanced multi leg",
			tx: (&Transfer{Reference: "ord-2"}).
				Debit("cash", try(9000)).
				Debit("fees", try(1000)).
				Credit("revenue", try(10000)),
		},
		{
			name: "multi currency balances per currency",
			tx: (&Transfer{Reference: "fx-1"}).
				Debit("try_wallet", try(10000)).
				Credit("try_clearing", try(10000)).
				Debit("usd_clearing", Amount(300, "USD")).
				Credit("usd_wallet", Amount(300, "USD")),
		},
		{
			name: "off by one kurus",
			tx:   (&Transfer{Reference: "ord-3"}).Debit("cash", try(10000)).Credit("revenue", try(9999)),
			want: ErrUnbalanced,
		},
		{
			name: "one currency balances, the other does not",
			tx: (&Transfer{Reference: "fx-2"}).
				Debit("try_wallet", try(10000)).
				Credit("try_clearing", try(10000)).
				Debit("usd_clearing", Amount(300, "USD")).
				Credit("usd_wallet", Amount(299, "USD")),
			want: ErrUnbalanced,
		},
		{
			name: "single posting",
			tx:   (&Transfer{Reference: "ord-4"}).Debit("cash", try(10000)),
			want: ErrTooFewPostings,
		},
		{
			name: "no reference",
			tx:   (&Transfer{}).Debit("cash", try(10000)).Credit("revenue", try(10000)),
			want: ErrMissingReference,
		},
		{
			name: "zero amount",
			tx:   (&Transfer{Reference: "ord-5"}).Debit("cash", try(0)).Credit("revenue", try(0)),
			want: ErrNonPositiveAmount,
		},
		{
			name: "negative amount instead of credit",
			tx:   (&Transfer{Reference: "ord-6"}).Debit("cash", try(-10000)).Debit("revenue", try(10000)),
			want: ErrNonPositiveAmount,
		},
		{
			name: "lowercase currency",
			tx:   (&Transfer{Reference: "ord-7"}).Debit("cash", Amount(100, "try")).Credit("revenue", Amount(100, "try")),
			want: ErrInvalidCurrency,
		},
		{
			name: "empty account id",
			tx:   (&Transfer{Reference: "ord-8"}).Debit("", try(100)).Credit("revenue", try(100)),
			want: ErrInvalidAccount,
		},
		{
			name: "bad direction",
			tx: &Transfer{Reference: "ord-9", Postings: []Posting{
				{Account: "cash", Amount: try(100), Dir: "sideways"},
				{Account: "revenue", Amount: try(100), Dir: Credit},
			}},
			want: ErrInvalidDirection,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.tx.Validate()
			if !errors.Is(err, tt.want) {
				t.Fatalf("Validate() = %v, want %v", err, tt.want)
			}
		})
	}
}

// A transfer built as N debits and M credits drawn from the same pool of minor
// units must always balance, whatever the split. This is the property the whole
// product rests on, so it gets random input rather than a handful of examples.
func TestBalancedSplitsAlwaysNetZero(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for i := 0; i < 2000; i++ {
		total := rng.Int63n(1_000_000) + 2
		tx := &Transfer{Reference: "prop"}

		for _, side := range []Direction{Debit, Credit} {
			left, legs := total, rng.Intn(4)+1
			for leg := 0; leg < legs; leg++ {
				amount := left
				if leg < legs-1 {
					// Leave at least one kuruş for each remaining leg.
					amount = rng.Int63n(left-int64(legs-leg-1)) + 1
				}
				left -= amount
				p := Posting{Account: "acc", Amount: try(amount), Dir: side}
				tx.Postings = append(tx.Postings, p)
			}
			if left != 0 {
				t.Fatalf("test bug: %s split left %d unallocated", side, left)
			}
		}

		if err := tx.Validate(); err != nil {
			t.Fatalf("balanced split rejected: %v (postings %+v)", err, tx.Postings)
		}
	}
}

// The mirror of the property above: perturbing exactly one leg by one minor
// unit must always be caught. An off-by-one kuruş is the failure mode that
// actually happens in production.
func TestOneKurusDriftIsAlwaysCaught(t *testing.T) {
	rng := rand.New(rand.NewSource(2))

	for i := 0; i < 1000; i++ {
		amount := rng.Int63n(1_000_000) + 1
		tx := (&Transfer{Reference: "prop"}).
			Debit("cash", try(amount)).
			Credit("revenue", try(amount+1))

		if err := tx.Validate(); !errors.Is(err, ErrUnbalanced) {
			t.Fatalf("drift not caught for %d: %v", amount, err)
		}
	}
}
