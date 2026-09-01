package ledger

import (
	"encoding/json"
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

// UnmarshalJSON accepts only the string form. A bare JSON number is rejected
// rather than accepted-and-rounded: if a client is sending money as a number,
// the right time to find out is now.
func (m *Money) UnmarshalJSON(b []byte) error {
	var raw struct {
		Amount   json.RawMessage `json:"amount"`
		Currency string          `json:"currency"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw.Amount) == 0 || raw.Amount[0] != '"' {
		return fmt.Errorf("sumzero: amount must be a string of minor units, got %s", raw.Amount)
	}

	var digits string
	if err := json.Unmarshal(raw.Amount, &digits); err != nil {
		return err
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return fmt.Errorf("sumzero: amount %q is not an integer: %w", digits, err)
	}

	m.Amount, m.Currency = n, raw.Currency
	return nil
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
