package ledger_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/YusufDrymz/sumzero/ledger"
)

// guardedBooks opens a wallet that may not go negative, funded with 10000.
func guardedBooks(t *testing.T) *ledger.Ledger {
	t.Helper()
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	for _, a := range []ledger.Account{
		{ID: "wallet", Type: ledger.Asset, Currency: "TRY", AllowNegative: false},
		{ID: "merchant", Type: ledger.Liability, Currency: "TRY", AllowNegative: true},
		{ID: "topup", Type: ledger.Liability, Currency: "TRY", AllowNegative: true},
	} {
		require.NoError(t, lg.CreateAccount(ctx, a))
	}
	_, err := lg.Post(ctx, (&ledger.Transfer{Reference: "fund"}).
		Debit("wallet", try(10000)).Credit("topup", try(10000)))
	require.NoError(t, err)
	return lg
}

func pay(ref string, minor int64) *ledger.Transfer {
	return (&ledger.Transfer{Reference: ref}).
		Credit("wallet", try(minor)).Debit("merchant", try(minor))
}

func TestOverdraftGuard(t *testing.T) {
	ctx := context.Background()
	lg := guardedBooks(t)

	_, err := lg.Post(ctx, pay("spend-ok", 10000))
	require.NoError(t, err, "spending exactly the balance is fine")

	_, err = lg.Post(ctx, pay("spend-over", 1))
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)

	bal, err := lg.Balance(ctx, "wallet")
	require.NoError(t, err)
	require.Equal(t, int64(0), bal.Amount, "the refused transfer left no trace")

	// The guard is per account: the merchant may go negative all it likes.
	_, err = lg.Post(ctx, (&ledger.Transfer{Reference: "refund"}).
		Debit("wallet", try(500)).Credit("merchant", try(500)))
	require.NoError(t, err)
}

func TestHoldReservesAvailableBalance(t *testing.T) {
	ctx := context.Background()
	lg := guardedBooks(t)

	h, err := lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth-1", Amount: try(6000)})
	require.NoError(t, err)
	require.Equal(t, ledger.HoldActive, h.Status)

	avail, err := lg.Available(ctx, "wallet")
	require.NoError(t, err)
	require.Equal(t, int64(4000), avail.Amount)

	bal, err := lg.Balance(ctx, "wallet")
	require.NoError(t, err)
	require.Equal(t, int64(10000), bal.Amount, "a hold moves nothing")

	// The reservation binds both new holds and plain transfers.
	_, err = lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth-2", Amount: try(4001)})
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
	_, err = lg.Post(ctx, pay("spend", 4001))
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
	_, err = lg.Post(ctx, pay("spend-within", 4000))
	require.NoError(t, err)

	// Release gives it back.
	require.NoError(t, lg.Release(ctx, "auth-1"))
	avail, err = lg.Available(ctx, "wallet")
	require.NoError(t, err)
	require.Equal(t, int64(6000), avail.Amount)

	require.ErrorIs(t, lg.Release(ctx, "auth-1"), ledger.ErrHoldNotActive)
	_, err = lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth-1", Amount: try(1)})
	require.ErrorIs(t, err, ledger.ErrDuplicateReference)
}

func TestCapture(t *testing.T) {
	ctx := context.Background()

	t.Run("full capture", func(t *testing.T) {
		lg := guardedBooks(t)
		_, err := lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth", Amount: try(7000)})
		require.NoError(t, err)

		id, err := lg.Capture(ctx, "auth", pay("cap", 7000))
		require.NoError(t, err)

		h, err := lg.HoldByReference(ctx, "auth")
		require.NoError(t, err)
		require.Equal(t, ledger.HoldCaptured, h.Status)
		require.NotNil(t, h.TransferID)
		require.Equal(t, id, *h.TransferID)

		avail, err := lg.Available(ctx, "wallet")
		require.NoError(t, err)
		require.Equal(t, int64(3000), avail.Amount)
	})

	t.Run("partial capture releases the rest", func(t *testing.T) {
		lg := guardedBooks(t)
		_, err := lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth", Amount: try(7000)})
		require.NoError(t, err)

		_, err = lg.Capture(ctx, "auth", pay("cap", 2500))
		require.NoError(t, err)

		avail, err := lg.Available(ctx, "wallet")
		require.NoError(t, err)
		require.Equal(t, int64(7500), avail.Amount, "10000 - 2500 spent, nothing still held")
	})

	t.Run("capture beyond the hold is refused", func(t *testing.T) {
		lg := guardedBooks(t)
		_, err := lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth", Amount: try(7000)})
		require.NoError(t, err)

		_, err = lg.Capture(ctx, "auth", pay("cap", 7001))
		require.ErrorIs(t, err, ledger.ErrCaptureMismatch)

		h, err := lg.HoldByReference(ctx, "auth")
		require.NoError(t, err)
		require.Equal(t, ledger.HoldActive, h.Status, "failed capture leaves the hold as it was")
	})

	t.Run("capture that does not draw on the account is refused", func(t *testing.T) {
		lg := guardedBooks(t)
		_, err := lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth", Amount: try(100)})
		require.NoError(t, err)

		_, err = lg.Capture(ctx, "auth", (&ledger.Transfer{Reference: "elsewhere"}).
			Debit("merchant", try(100)).Credit("topup", try(100)))
		require.ErrorIs(t, err, ledger.ErrCaptureMismatch)
	})

	t.Run("capture may spend the held amount even when nothing else is free", func(t *testing.T) {
		lg := guardedBooks(t)
		_, err := lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth", Amount: try(10000)})
		require.NoError(t, err)

		// Available is 0, but the hold itself covers the capture.
		_, err = lg.Capture(ctx, "auth", pay("cap", 10000))
		require.NoError(t, err)
	})

	t.Run("captured hold cannot be captured or released again", func(t *testing.T) {
		lg := guardedBooks(t)
		_, err := lg.Hold(ctx, ledger.HoldRequest{Account: "wallet", Reference: "auth", Amount: try(100)})
		require.NoError(t, err)
		_, err = lg.Capture(ctx, "auth", pay("cap", 100))
		require.NoError(t, err)

		_, err = lg.Capture(ctx, "auth", pay("cap-2", 100))
		require.ErrorIs(t, err, ledger.ErrHoldNotActive)
		require.ErrorIs(t, lg.Release(ctx, "auth"), ledger.ErrHoldNotActive)
	})
}

func TestHoldExpiry(t *testing.T) {
	ctx := context.Background()
	lg := guardedBooks(t)

	_, err := lg.Hold(ctx, ledger.HoldRequest{
		Account: "wallet", Reference: "short", Amount: try(6000),
		ExpiresAt: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)
	_, err = lg.Hold(ctx, ledger.HoldRequest{
		Account: "wallet", Reference: "long", Amount: try(3000),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	// Until the sweep runs, even a past-due hold still reserves.
	avail, err := lg.Available(ctx, "wallet")
	require.NoError(t, err)
	require.Equal(t, int64(1000), avail.Amount)

	n, err := lg.ExpireHolds(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	avail, err = lg.Available(ctx, "wallet")
	require.NoError(t, err)
	require.Equal(t, int64(7000), avail.Amount)

	h, err := lg.HoldByReference(ctx, "short")
	require.NoError(t, err)
	require.Equal(t, ledger.HoldExpired, h.Status)
	_, err = lg.Capture(ctx, "short", pay("late", 1))
	require.ErrorIs(t, err, ledger.ErrHoldNotActive)
}

// Two holds racing for the last of the balance: exactly one may win, and the
// wallet must never end up over-reserved.
func TestConcurrentHoldsNeverOverReserve(t *testing.T) {
	ctx := context.Background()
	lg := guardedBooks(t)

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = lg.Hold(ctx, ledger.HoldRequest{
				Account: "wallet", Reference: "race-" + string(rune('a'+i)), Amount: try(6000),
			})
		}(i)
	}
	wg.Wait()

	var ok int
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
		}
	}
	require.Equal(t, 1, ok, "10000 covers one hold of 6000, never two")

	avail, err := lg.Available(ctx, "wallet")
	require.NoError(t, err)
	require.Equal(t, int64(4000), avail.Amount)
}
