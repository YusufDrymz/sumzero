package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/YusufDrymz/sumzero/ledger"
)

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)

	day := func(d int) time.Time { return time.Date(2026, 6, d, 12, 0, 0, 0, time.UTC) }
	post := func(ref string, d int, minor int64) {
		t.Helper()
		_, err := lg.Post(ctx, &ledger.Transfer{Reference: ref, PostedAt: day(d),
			Postings: (&ledger.Transfer{}).Debit("cash", try(minor)).Credit("revenue", try(minor)).Postings})
		require.NoError(t, err)
	}
	post("pay-1", 2, 10000) // matches
	post("pay-2", 3, 5000)  // bank says 4990: fee taken, nobody recorded it
	post("pay-3", 4, 7000)  // never arrived at the bank
	post("pay-9", 20, 100)  // outside the window, must be ignored

	// A refund: cash goes down. Ledger side nets negative on an asset account,
	// external side is negative too.
	_, err := lg.Post(ctx, &ledger.Transfer{Reference: "refund-1", PostedAt: day(5),
		Postings: (&ledger.Transfer{}).Credit("cash", try(2000)).Debit("revenue", try(2000)).Postings})
	require.NoError(t, err)

	external := []ledger.ExternalEntry{
		{Reference: "pay-1", Amount: try(10000), Date: day(2)},
		{Reference: "pay-2", Amount: try(4990), Date: day(3)},
		{Reference: "refund-1", Amount: try(-2000), Date: day(5)},
		{Reference: "pay-7", Amount: try(3000), Date: day(6)}, // arrived, never recorded
	}

	r, err := lg.Reconcile(ctx, "cash", day(1), day(10), external)
	require.NoError(t, err)
	require.False(t, r.Clean())

	refs := func(ms []ledger.Match) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = m.Reference
		}
		return out
	}
	require.ElementsMatch(t, []string{"pay-1", "refund-1"}, refs(r.Matched))
	require.ElementsMatch(t, []string{"pay-2"}, refs(r.AmountMismatch))
	require.Equal(t, int64(5000), r.AmountMismatch[0].Ledger.Amount)
	require.Equal(t, int64(4990), r.AmountMismatch[0].External.Amount)

	require.Len(t, r.MissingInLedger, 1)
	require.Equal(t, "pay-7", r.MissingInLedger[0].Reference)

	require.Len(t, r.MissingExternally, 1)
	require.Equal(t, "pay-3", r.MissingExternally[0].Reference)

	// Ledger inside the window: 10000 + 5000 + 7000 - 2000 = 20000.
	// External: 10000 + 4990 - 2000 + 3000 = 15990.
	require.Equal(t, int64(20000), r.LedgerTotal)
	require.Equal(t, int64(15990), r.ExternalTotal)
	require.Equal(t, int64(4010), r.Difference)

	// Every entry is in exactly one bucket.
	total := len(r.Matched) + len(r.AmountMismatch) + len(r.MissingInLedger) + len(r.MissingExternally)
	require.Equal(t, 5, total, "4 external + 1 ledger-only, no double counting")
}

func TestReconcileCleanBooks(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)

	at := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	_, err := lg.Post(ctx, &ledger.Transfer{Reference: "ok-1", PostedAt: at,
		Postings: (&ledger.Transfer{}).Debit("cash", try(100)).Credit("revenue", try(100)).Postings})
	require.NoError(t, err)

	r, err := lg.Reconcile(ctx, "cash", at.Add(-time.Hour), at.Add(time.Hour),
		[]ledger.ExternalEntry{{Reference: "ok-1", Amount: try(100)}})
	require.NoError(t, err)
	require.True(t, r.Clean())
	require.Equal(t, int64(0), r.Difference)
	// Empty buckets serialise as [] not null.
	require.NotNil(t, r.MissingInLedger)
	require.NotNil(t, r.MissingExternally)
}

// A credit-normal account (revenue) must reconcile with the same sign
// convention as an asset: positive means "more of what this account holds".
func TestReconcileRespectsNormalSide(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)

	at := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	_, err := lg.Post(ctx, &ledger.Transfer{Reference: "sale-1", PostedAt: at,
		Postings: (&ledger.Transfer{}).Debit("cash", try(800)).Credit("revenue", try(800)).Postings})
	require.NoError(t, err)

	r, err := lg.Reconcile(ctx, "revenue", at.Add(-time.Hour), at.Add(time.Hour),
		[]ledger.ExternalEntry{{Reference: "sale-1", Amount: try(800)}})
	require.NoError(t, err)
	require.True(t, r.Clean(), "revenue of 800 must match external +800: %+v", r)
}

func TestReconcileRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)
	at := time.Now()

	tests := []struct {
		name     string
		external []ledger.ExternalEntry
		want     error
	}{
		{"duplicate reference in file", []ledger.ExternalEntry{
			{Reference: "x", Amount: try(1)}, {Reference: "x", Amount: try(1)}}, ledger.ErrDuplicateReference},
		{"wrong currency", []ledger.ExternalEntry{
			{Reference: "x", Amount: ledger.Amount(1, "USD")}}, ledger.ErrCurrencyMismatch},
		{"no reference", []ledger.ExternalEntry{
			{Reference: "", Amount: try(1)}}, ledger.ErrMissingReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lg.Reconcile(ctx, "cash", at.Add(-time.Hour), at, tt.external)
			require.ErrorIs(t, err, tt.want)
		})
	}

	_, err := lg.Reconcile(ctx, "ghost", at.Add(-time.Hour), at, nil)
	require.ErrorIs(t, err, ledger.ErrUnknownAccount)
}
