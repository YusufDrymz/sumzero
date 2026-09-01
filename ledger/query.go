package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Balance returns the current balance of an account, signed so that a positive
// number means "more of what this account normally holds": cash you have, debt
// you owe, revenue you earned.
func (l *Ledger) Balance(ctx context.Context, accountID string) (Money, error) {
	var (
		raw      int64
		typ      AccountType
		currency string
	)
	err := l.db.QueryRow(ctx, `
		SELECT b.balance, a.type, a.currency
		FROM account_balances b JOIN accounts a ON a.id = b.account_id
		WHERE b.account_id = $1`, accountID).Scan(&raw, &typ, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return Money{}, fmt.Errorf("%w: %s", ErrUnknownAccount, accountID)
	}
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: normalise(raw, typ), Currency: currency}, nil
}

// BalanceAsOf recomputes a balance from the postings up to and including at.
//
// It reads history rather than the cached balance, so it stays correct for
// backdated transfers — which is the whole reason `posted_at` exists.
func (l *Ledger) BalanceAsOf(ctx context.Context, accountID string, at time.Time) (Money, error) {
	acc, err := l.Account(ctx, accountID)
	if err != nil {
		return Money{}, err
	}

	var raw int64
	err = l.db.QueryRow(ctx, `
		SELECT coalesce(sum(CASE p.direction WHEN 'debit' THEN p.amount ELSE -p.amount END), 0)
		FROM postings p JOIN transfers t ON t.id = p.transfer_id
		WHERE p.account_id = $1 AND t.posted_at <= $2`, accountID, at).Scan(&raw)
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: normalise(raw, acc.Type), Currency: acc.Currency}, nil
}

// Entry is one line of an account statement.
type Entry struct {
	TransferID  int64     `json:"transfer_id"`
	Reference   string    `json:"reference"`
	Description string    `json:"description"`
	PostedAt    time.Time `json:"posted_at"`
	Amount      Money     `json:"amount"`
	Dir         Direction `json:"direction"`
}

// StatementOptions narrows a statement. The zero value means "everything, most
// recent first".
type StatementOptions struct {
	From  time.Time
	To    time.Time
	Limit int
}

// Statement lists an account's postings in ledger-date order, newest first.
func (l *Ledger) Statement(ctx context.Context, accountID string, opt StatementOptions) ([]Entry, error) {
	if _, err := l.Account(ctx, accountID); err != nil {
		return nil, err
	}
	limit := opt.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	// The id tiebreaker keeps the order stable when several transfers share a
	// posted_at, which happens constantly with backdated batches.
	rows, err := l.db.Query(ctx, `
		SELECT t.id, t.reference, t.description, t.posted_at, p.amount, p.currency, p.direction
		FROM postings p JOIN transfers t ON t.id = p.transfer_id
		WHERE p.account_id = $1
		  AND ($2::timestamptz IS NULL OR t.posted_at >= $2)
		  AND ($3::timestamptz IS NULL OR t.posted_at <= $3)
		ORDER BY t.posted_at DESC, t.id DESC
		LIMIT $4`, accountID, nullTime(opt.From), nullTime(opt.To), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.TransferID, &e.Reference, &e.Description, &e.PostedAt,
			&e.Amount.Amount, &e.Amount.Currency, &e.Dir); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrialBalance sums every account's stored balance per currency. In a ledger
// that has only ever seen balanced transfers, each currency sums to zero.
func (l *Ledger) TrialBalance(ctx context.Context) (map[string]int64, error) {
	rows, err := l.db.Query(ctx, `
		SELECT a.currency, sum(b.balance)
		FROM account_balances b JOIN accounts a ON a.id = b.account_id
		GROUP BY a.currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var cur string
		var sum int64
		if err := rows.Scan(&cur, &sum); err != nil {
			return nil, err
		}
		out[cur] = sum
	}
	return out, rows.Err()
}

// normalise flips the stored debit-positive number for credit-normal accounts.
func normalise(raw int64, t AccountType) int64 {
	if t.debitNormal() {
		return raw
	}
	return -raw
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// TransferByReference returns a posted transfer with its postings.
func (l *Ledger) TransferByReference(ctx context.Context, reference string) (Transfer, int64, error) {
	var (
		id int64
		t  Transfer
	)
	var reverses *int64
	err := l.db.QueryRow(ctx, `
		SELECT id, reference, description, posted_at, reverses_transfer_id
		FROM transfers WHERE reference = $1`,
		reference).Scan(&id, &t.Reference, &t.Description, &t.PostedAt, &reverses)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, 0, fmt.Errorf("%w: %s", ErrUnknownTransfer, reference)
	}
	if err != nil {
		return Transfer{}, 0, err
	}
	if reverses != nil {
		t.Reverses = *reverses
	}

	rows, err := l.db.Query(ctx, `
		SELECT account_id, amount, currency, direction FROM postings
		WHERE transfer_id = $1 ORDER BY id`, id)
	if err != nil {
		return Transfer{}, 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var p Posting
		if err := rows.Scan(&p.Account, &p.Amount.Amount, &p.Amount.Currency, &p.Dir); err != nil {
			return Transfer{}, 0, err
		}
		t.Postings = append(t.Postings, p)
	}
	return t, id, rows.Err()
}
