package ledger

import "errors"

var (
	ErrInvalidAccount     = errors.New("sumzero: invalid account id")
	ErrInvalidType        = errors.New("sumzero: invalid account type")
	ErrInvalidCurrency    = errors.New("sumzero: invalid currency code")
	ErrAccountExists      = errors.New("sumzero: account already exists")
	ErrUnknownAccount     = errors.New("sumzero: unknown account")
	ErrAccountArchived    = errors.New("sumzero: account is archived")
	ErrTooFewPostings     = errors.New("sumzero: transfer needs at least two postings")
	ErrUnbalanced         = errors.New("sumzero: transfer does not balance")
	ErrCurrencyMismatch   = errors.New("sumzero: posting currency does not match account")
	ErrNonPositiveAmount  = errors.New("sumzero: posting amount must be positive")
	ErrInvalidDirection   = errors.New("sumzero: invalid posting direction")
	ErrMissingReference   = errors.New("sumzero: transfer reference is required")
	ErrDuplicateReference = errors.New("sumzero: transfer reference already posted")
)
