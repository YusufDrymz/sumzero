package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// db is the slice of pgx that this package needs. Both *pgxpool.Pool and
// pgx.Tx satisfy it, which is what lets a caller post inside their own
// transaction.
type db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Ledger reads and writes ledger tables in a Postgres database.
type Ledger struct {
	db db

	// pool is set only when the Ledger owns its connections. With a caller's
	// transaction it is nil and Post joins that transaction instead of opening
	// its own.
	pool *pgxpool.Pool
}

// New returns a Ledger that manages its own transactions.
func New(pool *pgxpool.Pool) *Ledger {
	return &Ledger{db: pool, pool: pool}
}

// NewTx returns a Ledger that runs inside the caller's transaction. Post does
// not commit; the caller does, together with its own writes. This is the point
// of the package: the ledger entry and the thing it describes land atomically
// or not at all.
func NewTx(tx pgx.Tx) *Ledger {
	return &Ledger{db: tx}
}

// CreateAccount registers an account. Currency is fixed for its lifetime.
func (l *Ledger) CreateAccount(ctx context.Context, a Account) error {
	if a.ID == "" {
		return ErrInvalidAccount
	}
	if !a.Type.valid() {
		return fmt.Errorf("%w: %q", ErrInvalidType, a.Type)
	}
	if !validCurrency(a.Currency) {
		return fmt.Errorf("%w: %q", ErrInvalidCurrency, a.Currency)
	}

	return l.inTx(ctx, func(q db) error {
		tag, err := q.Exec(ctx, `
			INSERT INTO accounts (id, type, currency, allow_negative) VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO NOTHING`, a.ID, string(a.Type), a.Currency, a.AllowNegative)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s", ErrAccountExists, a.ID)
		}
		_, err = q.Exec(ctx, `INSERT INTO account_balances (account_id) VALUES ($1)`, a.ID)
		return err
	})
}

// Account returns one account.
func (l *Ledger) Account(ctx context.Context, id string) (Account, error) {
	var a Account
	err := l.db.QueryRow(ctx,
		`SELECT id, type, currency, archived, allow_negative FROM accounts WHERE id = $1`, id).
		Scan(&a.ID, &a.Type, &a.Currency, &a.Archived, &a.AllowNegative)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: %s", ErrUnknownAccount, id)
	}
	return a, err
}

// Archive closes an account for new postings. History is kept; accounts are
// never deleted, because a deleted account makes every past statement a lie.
func (l *Ledger) Archive(ctx context.Context, id string) error {
	tag, err := l.db.Exec(ctx, `UPDATE accounts SET archived = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrUnknownAccount, id)
	}
	return nil
}

// Post validates and writes a transfer. It returns the assigned transfer id.
//
// Everything happens in one transaction: the postings, the balance updates and
// the hash chain link. A transfer that fails any check leaves no trace.
func (l *Ledger) Post(ctx context.Context, t *Transfer) (int64, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}
	var id int64
	err := l.inTx(ctx, func(q db) (err error) {
		id, err = l.post(ctx, q, t, 0)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// post is Post inside an open transaction, for a transfer that has already
// passed Validate. excludeHold is the hold this transfer captures, if any: it
// must not count against the account it is releasing.
func (l *Ledger) post(ctx context.Context, q db, t *Transfer, excludeHold int64) (int64, error) {
	// Store the same instant the chain hashes: Postgres keeps microseconds, so
	// anything finer would be lost on write and break verification later.
	postedAt := t.PostedAt
	if postedAt.IsZero() {
		postedAt = time.Now()
	}
	postedAt = postedAt.UTC().Truncate(time.Microsecond)

	// Hand the resolved instant back to the caller: it is what went into the
	// row and the hash, and a response that reports a zero time is a lie.
	t.PostedAt = postedAt

	accounts, err := l.lockAccounts(ctx, q, t)
	if err != nil {
		return 0, err
	}
	for i, p := range t.Postings {
		acc, ok := accounts[p.Account]
		if !ok {
			return 0, fmt.Errorf("%w: %s", ErrUnknownAccount, p.Account)
		}
		if acc.Archived {
			return 0, fmt.Errorf("%w: %s", ErrAccountArchived, p.Account)
		}
		if acc.Currency != p.Amount.Currency {
			return 0, fmt.Errorf("%w: posting %d: account %s is %s, posting is %s",
				ErrCurrencyMismatch, i, p.Account, acc.Currency, p.Amount.Currency)
		}
	}

	// The chain is a single global sequence, so links must be taken one at
	// a time. This serialises writes on purpose: a chain with a race in it
	// proves nothing. See docs/adr/0003. The lock lives until the
	// transaction ends — in embedded mode that is the caller's commit, so a
	// caller that posts early and commits late stalls every other writer.
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, chainLockKey); err != nil {
		return 0, err
	}

	var prev []byte
	err = q.QueryRow(ctx, `SELECT hash FROM transfers ORDER BY id DESC LIMIT 1`).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	var reverses any
	if t.Reverses != 0 {
		reverses = t.Reverses
	}
	var id int64
	err = q.QueryRow(ctx, `
		INSERT INTO transfers (reference, description, posted_at, prev_hash, hash, reverses_transfer_id)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		t.Reference, t.Description, postedAt, prev, chainDigest(prev, t, postedAt), reverses).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			if t.Reverses != 0 && uniqueViolationOn(err, "transfers_reverses_key") {
				return 0, fmt.Errorf("%w: transfer %d", ErrAlreadyReversed, t.Reverses)
			}
			return 0, fmt.Errorf("%w: %s", ErrDuplicateReference, t.Reference)
		}
		return 0, err
	}

	for _, p := range t.Postings {
		_, err = q.Exec(ctx, `
			INSERT INTO postings (transfer_id, account_id, amount, currency, direction)
			VALUES ($1, $2, $3, $4, $5)`,
			id, p.Account, p.Amount.Amount, p.Amount.Currency, string(p.Dir))
		if err != nil {
			return 0, err
		}
	}

	if err := l.applyBalances(ctx, q, t); err != nil {
		return 0, err
	}

	// The overdraft guard runs after the balances moved, on the same locked
	// rows: an account that may not go negative must still cover its active
	// holds. Failing here rolls the whole transfer back.
	for _, acc := range accounts {
		if acc.AllowNegative {
			continue
		}
		avail, err := availableLocked(ctx, q, acc, excludeHold)
		if err != nil {
			return 0, err
		}
		if avail < 0 {
			return 0, fmt.Errorf("%w: %s would be %d after %s",
				ErrInsufficientFunds, acc.ID, avail, t.Reference)
		}
	}
	return id, nil
}

// lockAccounts reads every account the transfer touches and holds a row lock on
// each, in a stable order. Sorting by id is what keeps two concurrent transfers
// over the same pair of accounts from deadlocking each other.
func (l *Ledger) lockAccounts(ctx context.Context, q db, t *Transfer) (map[string]Account, error) {
	ids := make([]string, 0, len(t.Postings))
	seen := make(map[string]bool, len(t.Postings))
	for _, p := range t.Postings {
		if !seen[p.Account] {
			seen[p.Account] = true
			ids = append(ids, p.Account)
		}
	}

	rows, err := q.Query(ctx, `
		SELECT id, type, currency, archived, allow_negative FROM accounts
		WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]Account, len(ids))
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Type, &a.Currency, &a.Archived, &a.AllowNegative); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

// applyBalances folds the transfer into the materialised balances. Amounts are
// summed per account first so a transfer touching one account twice produces a
// single update. It upserts rather than updates: an account created with plain
// SQL has no balance row yet, and a silent zero-row UPDATE would leave the
// cache drifting until the next verify.
func (l *Ledger) applyBalances(ctx context.Context, q db, t *Transfer) error {
	delta := make(map[string]int64, len(t.Postings))
	order := make([]string, 0, len(t.Postings))
	for _, p := range t.Postings {
		if _, ok := delta[p.Account]; !ok {
			order = append(order, p.Account)
		}
		delta[p.Account] += p.Dir.signed(p.Amount.Amount)
	}

	for _, acc := range order {
		if delta[acc] == 0 {
			continue
		}
		_, err := q.Exec(ctx, `
			INSERT INTO account_balances (account_id, balance) VALUES ($1, $2)
			ON CONFLICT (account_id) DO UPDATE
			SET balance = account_balances.balance + excluded.balance, updated_at = now()`,
			acc, delta[acc])
		if err != nil {
			return err
		}
	}
	return nil
}

// inTx runs fn in a transaction. With a caller-supplied transaction it runs fn
// directly, leaving commit and rollback to the caller.
func (l *Ledger) inTx(ctx context.Context, fn func(db) error) error {
	if l.pool == nil {
		return fn(l.db)
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after a successful commit

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// chainLockKey is an arbitrary but fixed advisory-lock id for the hash chain.
const chainLockKey int64 = 0x53554D5A45524F31 // "SUMZERO1"

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func uniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == constraint
}

// Reverse posts the mirror image of an earlier transfer: same accounts, same
// amounts, every leg flipped. The new transfer is linked to the original and
// the original can only be reversed once. The overdraft guard applies as it
// would to any transfer — undoing a payout into a guarded wallet is fine,
// undoing a top-up out of an empty one is not.
func (l *Ledger) Reverse(ctx context.Context, reference, newReference, description string) (int64, error) {
	if newReference == "" {
		return 0, ErrMissingReference
	}
	orig, origID, err := l.TransferByReference(ctx, reference)
	if err != nil {
		return 0, err
	}
	if description == "" {
		description = "reversal of " + reference
	}

	rev := &Transfer{Reference: newReference, Description: description, Reverses: origID}
	for _, p := range orig.Postings {
		dir := Credit
		if p.Dir == Credit {
			dir = Debit
		}
		rev.Postings = append(rev.Postings, Posting{Account: p.Account, Amount: p.Amount, Dir: dir})
	}
	return l.Post(ctx, rev)
}
