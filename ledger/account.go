package ledger

// AccountType classifies an account and fixes the side on which it carries a
// positive balance.
type AccountType string

const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Equity    AccountType = "equity"
	Income    AccountType = "income"
	Expense   AccountType = "expense"
)

func (t AccountType) valid() bool {
	switch t {
	case Asset, Liability, Equity, Income, Expense:
		return true
	}
	return false
}

// debitNormal reports whether a positive balance sits on the debit side.
func (t AccountType) debitNormal() bool {
	return t == Asset || t == Expense
}

// Account is a ledger account. Currency is fixed at creation: one account
// holds one currency, so a balance never needs an exchange rate to be read.
type Account struct {
	ID       string      `json:"id"`
	Type     AccountType `json:"type"`
	Currency string      `json:"currency"`
	Archived bool        `json:"archived"`

	// AllowNegative is the overdraft switch. True (the default) means the
	// ledger records whatever it is told, like a journal. False turns on the
	// guard: a transfer or hold that would push the available balance below
	// zero is refused.
	AllowNegative bool `json:"allow_negative"`
}

// Direction is the side a posting hits.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

func (d Direction) valid() bool { return d == Debit || d == Credit }

// signed returns the amount as it moves the balance in debit-positive terms.
func (d Direction) signed(minor int64) int64 {
	if d == Credit {
		return -minor
	}
	return minor
}
