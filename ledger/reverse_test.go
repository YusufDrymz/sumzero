package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/YusufDrymz/sumzero/ledger"
)

func TestReverse(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)

	_, err := lg.Post(ctx, (&ledger.Transfer{Reference: "sale", Description: "sale"}).
		Debit("cash", try(9000)).Debit("fees", try(1000)).Credit("revenue", try(10000)))
	require.NoError(t, err)

	revID, err := lg.Reverse(ctx, "sale", "sale-rev", "")
	require.NoError(t, err)

	rev, _, err := lg.TransferByReference(ctx, "sale-rev")
	require.NoError(t, err)
	require.Equal(t, "reversal of sale", rev.Description)
	require.NotZero(t, rev.Reverses)
	require.Len(t, rev.Postings, 3)
	require.Equal(t, ledger.Credit, rev.Postings[0].Dir, "debit legs come back as credits")
	require.Equal(t, ledger.Debit, rev.Postings[2].Dir)

	for _, acc := range []string{"cash", "fees", "revenue"} {
		bal, err := lg.Balance(ctx, acc)
		require.NoError(t, err)
		require.Equal(t, int64(0), bal.Amount, acc)
	}

	_, err = lg.Reverse(ctx, "sale", "sale-rev-2", "")
	require.ErrorIs(t, err, ledger.ErrAlreadyReversed, "an original is reversed once")

	// Reversing the reversal is a new transfer, and it restores the books.
	_, err = lg.Reverse(ctx, "sale-rev", "sale-rev-rev", "")
	require.NoError(t, err)
	bal, err := lg.Balance(ctx, "cash")
	require.NoError(t, err)
	require.Equal(t, int64(9000), bal.Amount)

	_, err = lg.Reverse(ctx, "ghost", "x", "")
	require.ErrorIs(t, err, ledger.ErrUnknownTransfer)

	// The link is part of the chain, and the chain still verifies with
	// pre-reversal transfers in it.
	r, err := lg.Verify(ctx)
	require.NoError(t, err)
	require.True(t, r.OK(), "%v", r.Problems)
	_ = revID
}

func TestReverseRespectsOverdraftGuard(t *testing.T) {
	ctx := context.Background()
	lg := guardedBooks(t) // wallet funded with 10000 by "fund"

	_, err := lg.Post(ctx, pay("spend", 8000))
	require.NoError(t, err)

	// Undoing the top-up would take 10000 out of a wallet holding 2000.
	_, err = lg.Reverse(ctx, "fund", "fund-rev", "")
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)

	// Undoing the spend puts money back; always fine.
	_, err = lg.Reverse(ctx, "spend", "spend-rev", "")
	require.NoError(t, err)
}

// A reversal's link is hashed, so rewriting which transfer it points at is
// caught even though the postings are untouched.
func TestReversalLinkIsTamperEvident(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	lg := ledger.New(pool)
	openBooks(t, lg)

	for _, ref := range []string{"a", "b"} {
		_, err := lg.Post(ctx, (&ledger.Transfer{Reference: ref}).
			Debit("cash", try(100)).Credit("revenue", try(100)))
		require.NoError(t, err)
	}
	_, err := lg.Reverse(ctx, "a", "a-rev", "")
	require.NoError(t, err)

	disableAppendOnly(t, pool)
	_, err = pool.Exec(ctx, `UPDATE transfers SET reverses_transfer_id = (SELECT id FROM transfers WHERE reference = 'b') WHERE reference = 'a-rev'`)
	require.NoError(t, err)

	r, err := lg.Verify(ctx)
	require.NoError(t, err)
	require.False(t, r.OK())
	require.Equal(t, "hash-mismatch", r.Problems[0].Kind)
}

// The sweep is lazy; capture must not trust it. A hold whose expiry has passed
// is expired the moment someone tries to use it.
func TestCaptureRefusesUnsweptExpiredHold(t *testing.T) {
	ctx := context.Background()
	lg := guardedBooks(t)

	_, err := lg.Hold(ctx, ledger.HoldRequest{
		Account: "wallet", Reference: "auth", Amount: try(1000),
		ExpiresAt: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)

	_, err = lg.Capture(ctx, "auth", pay("cap", 1000))
	require.ErrorIs(t, err, ledger.ErrHoldExpired)

	h, err := lg.HoldByReference(ctx, "auth")
	require.NoError(t, err)
	require.Equal(t, ledger.HoldExpired, h.Status, "the failed capture marked it")

	avail, err := lg.Available(ctx, "wallet")
	require.NoError(t, err)
	require.Equal(t, int64(10000), avail.Amount, "and it no longer reserves")
}
