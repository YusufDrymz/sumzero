package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// HoldStatus is the lifecycle of a hold. It only ever moves forward.
type HoldStatus string

const (
	HoldActive   HoldStatus = "active"
	HoldCaptured HoldStatus = "captured"
	HoldReleased HoldStatus = "released"
	HoldExpired  HoldStatus = "expired"
)

// Hold reserves part of an account's balance without moving it.
type Hold struct {
	ID        int64      `json:"id"`
	Account   string     `json:"account"`
	Reference string     `json:"reference"`
	Amount    Money      `json:"amount"`
	Status    HoldStatus `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`

	// TransferID is set once the hold is captured.
	TransferID *int64 `json:"transfer_id,omitempty"`
}

// HoldRequest describes a hold to place.
type HoldRequest struct {
	Account   string
	Reference string
	Amount    Money

	// ExpiresAt is optional. An expired hold stops counting against the
	// available balance once Sweep has run; until then it still reserves.
	ExpiresAt time.Time
}

// Hold reserves an amount. On an account that may not go negative, the hold is
// refused if the available balance cannot cover it.
func (l *Ledger) Hold(ctx context.Context, req HoldRequest) (Hold, error) {
	if req.Reference == "" {
		return Hold{}, ErrMissingReference
	}
	if req.Amount.Amount <= 0 {
		return Hold{}, ErrNonPositiveAmount
	}

	var h Hold
	err := l.inTx(ctx, func(q db) error {
		acc, err := lockAccount(ctx, q, req.Account)
		if err != nil {
			return err
		}
		if acc.Archived {
			return fmt.Errorf("%w: %s", ErrAccountArchived, acc.ID)
		}
		if acc.Currency != req.Amount.Currency {
			return fmt.Errorf("%w: account %s is %s, hold is %s",
				ErrCurrencyMismatch, acc.ID, acc.Currency, req.Amount.Currency)
		}
		if !acc.AllowNegative {
			avail, err := availableLocked(ctx, q, acc, 0)
			if err != nil {
				return err
			}
			if avail < req.Amount.Amount {
				return fmt.Errorf("%w: %s has %d available, hold wants %d",
					ErrInsufficientFunds, acc.ID, avail, req.Amount.Amount)
			}
		}

		var expires any
		if !req.ExpiresAt.IsZero() {
			expires = req.ExpiresAt.UTC().Truncate(time.Microsecond)
		}
		err = q.QueryRow(ctx, `
			INSERT INTO holds (account_id, reference, amount, currency, expires_at)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, account_id, reference, amount, currency, status, created_at, expires_at`,
			acc.ID, req.Reference, req.Amount.Amount, req.Amount.Currency, expires).
			Scan(&h.ID, &h.Account, &h.Reference, &h.Amount.Amount, &h.Amount.Currency,
				&h.Status, &h.CreatedAt, &h.ExpiresAt)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: hold %s", ErrDuplicateReference, req.Reference)
		}
		return err
	})
	return h, err
}

// HoldByReference returns one hold.
func (l *Ledger) HoldByReference(ctx context.Context, reference string) (Hold, error) {
	h, err := scanHold(l.db.QueryRow(ctx, `
		SELECT id, account_id, reference, amount, currency, status, created_at,
		       expires_at, closed_at, transfer_id
		FROM holds WHERE reference = $1`, reference))
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, fmt.Errorf("%w: %s", ErrUnknownHold, reference)
	}
	return h, err
}

// Capture closes a hold by posting the transfer that moves the money. The
// transfer must reduce the held account by at most the held amount; whatever
// is not captured is released. Both happen in one transaction, so there is no
// moment where the money has moved and the hold still reserves it.
func (l *Ledger) Capture(ctx context.Context, holdReference string, t *Transfer) (int64, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}

	var id int64
	err := l.inTx(ctx, func(q db) error {
		h, err := lockHold(ctx, q, holdReference)
		if err != nil {
			return err
		}
		if h.Status != HoldActive {
			return fmt.Errorf("%w: hold %s is %s", ErrHoldNotActive, holdReference, h.Status)
		}

		acc, err := lockAccount(ctx, q, h.Account)
		if err != nil {
			return err
		}
		// How much this transfer takes off the held account, on its normal
		// side. A capture that adds to the account, or touches it not at all,
		// is not a capture of this hold.
		var spent int64
		for _, p := range t.Postings {
			if p.Account == h.Account {
				spent -= normalise(p.Dir.signed(p.Amount.Amount), acc.Type)
			}
		}
		if spent <= 0 {
			return fmt.Errorf("%w: transfer does not draw on %s", ErrCaptureMismatch, h.Account)
		}
		if spent > h.Amount.Amount {
			return fmt.Errorf("%w: hold %s covers %d, transfer takes %d",
				ErrCaptureMismatch, holdReference, h.Amount.Amount, spent)
		}

		id, err = l.post(ctx, q, t, h.ID)
		if err != nil {
			return err
		}
		_, err = q.Exec(ctx, `
			UPDATE holds SET status = 'captured', closed_at = now(), transfer_id = $2
			WHERE id = $1`, h.ID, id)
		return err
	})
	return id, err
}

// Release cancels an active hold.
func (l *Ledger) Release(ctx context.Context, holdReference string) error {
	return l.inTx(ctx, func(q db) error {
		h, err := lockHold(ctx, q, holdReference)
		if err != nil {
			return err
		}
		if h.Status != HoldActive {
			return fmt.Errorf("%w: hold %s is %s", ErrHoldNotActive, holdReference, h.Status)
		}
		_, err = q.Exec(ctx, `UPDATE holds SET status = 'released', closed_at = now() WHERE id = $1`, h.ID)
		return err
	})
}

// ExpireHolds marks past-due holds expired and returns how many. Until it runs,
// an expired hold keeps reserving; run it from a ticker or a cron.
func (l *Ledger) ExpireHolds(ctx context.Context) (int64, error) {
	tag, err := l.db.Exec(ctx, `
		UPDATE holds SET status = 'expired', closed_at = now()
		WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Available returns what an account can still spend: its balance on the
// normal side minus every active hold.
func (l *Ledger) Available(ctx context.Context, accountID string) (Money, error) {
	acc, err := l.Account(ctx, accountID)
	if err != nil {
		return Money{}, err
	}
	avail, err := availableLocked(ctx, l.db, acc, 0)
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: avail, Currency: acc.Currency}, nil
}

// availableLocked computes the available balance from the current rows. It is
// called with the account row locked, so the number cannot move under it.
// excludeHold is the hold being captured right now, which must not count
// against the transfer that captures it.
func availableLocked(ctx context.Context, q db, acc Account, excludeHold int64) (int64, error) {
	var raw, held int64
	err := q.QueryRow(ctx, `
		SELECT coalesce((SELECT balance FROM account_balances WHERE account_id = $1), 0),
		       coalesce((SELECT sum(amount) FROM holds
		                 WHERE account_id = $1 AND status = 'active' AND id <> $2), 0)`,
		acc.ID, excludeHold).Scan(&raw, &held)
	if err != nil {
		return 0, err
	}
	return normalise(raw, acc.Type) - held, nil
}

func lockAccount(ctx context.Context, q db, id string) (Account, error) {
	var a Account
	err := q.QueryRow(ctx, `
		SELECT id, type, currency, archived, allow_negative FROM accounts
		WHERE id = $1 FOR UPDATE`, id).
		Scan(&a.ID, &a.Type, &a.Currency, &a.Archived, &a.AllowNegative)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: %s", ErrUnknownAccount, id)
	}
	return a, err
}

func lockHold(ctx context.Context, q db, reference string) (Hold, error) {
	h, err := scanHold(q.QueryRow(ctx, `
		SELECT id, account_id, reference, amount, currency, status, created_at,
		       expires_at, closed_at, transfer_id
		FROM holds WHERE reference = $1 FOR UPDATE`, reference))
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, fmt.Errorf("%w: %s", ErrUnknownHold, reference)
	}
	return h, err
}

func scanHold(row pgx.Row) (Hold, error) {
	var h Hold
	err := row.Scan(&h.ID, &h.Account, &h.Reference, &h.Amount.Amount, &h.Amount.Currency,
		&h.Status, &h.CreatedAt, &h.ExpiresAt, &h.ClosedAt, &h.TransferID)
	return h, err
}
