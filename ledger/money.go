package ledger

import (
	"fmt"
	"strconv"
)

// Money is an exact amount in minor units (kuruş, cents, …) tagged with an
// ISO 4217 code. Never a float: money that goes through a float has already
// lost the argument.
type Money struct {
	Amount   int64
	Currency string
}

// Amount builds a Money value from minor units.
func Amount(minor int64, currency string) Money {
	return Money{Amount: minor, Currency: currency}
}

func (m Money) IsZero() bool { return m.Amount == 0 }

// String renders minor units and the code, e.g. "10000 TRY". Major-unit
// formatting is locale work and stays with the caller.
func (m Money) String() string {
	return strconv.FormatInt(m.Amount, 10) + " " + m.Currency
}

// MarshalJSON emits the amount as a string. JSON numbers are float64 in most
// parsers, and a ledger that rounds someone's balance in transport is worse
// than no ledger.
func (m Money) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `{"amount":%q,"currency":%q}`, strconv.FormatInt(m.Amount, 10), m.Currency), nil
}

func validCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}
