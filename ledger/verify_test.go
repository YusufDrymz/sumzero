package ledger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/YusufDrymz/sumzero/ledger"
)

// seedBooks posts a few transfers and returns a pool plus the ledger over it.
func seedBooks(t *testing.T) (*pgxpool.Pool, *ledger.Ledger) {
	t.Helper()
	ctx := context.Background()
	pool := startPostgres(t)
	lg := ledger.New(pool)
	openBooks(t, lg)

	for _, ref := range []string{"v-1", "v-2", "v-3"} {
		_, err := lg.Post(ctx, (&ledger.Transfer{Reference: ref}).
			Debit("cash", try(3000)).
			Credit("revenue", try(2000)).
			Credit("payable", try(1000)))
		require.NoError(t, err)
	}
	return pool, lg
}

// disableAppendOnly drops the guard so a test can play the part of someone with
// superuser access rewriting history. That is exactly the attacker the hash
// chain exists for: the trigger stops accidents, the chain catches intent.
func disableAppendOnly(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"transfers", "postings"} {
		_, err := pool.Exec(context.Background(),
			`ALTER TABLE `+table+` DISABLE TRIGGER `+table+`_append_only`)
		require.NoError(t, err)
	}
}

func TestVerifyCleanLedger(t *testing.T) {
	ctx := context.Background()
	_, lg := seedBooks(t)

	r, err := lg.Verify(ctx)
	require.NoError(t, err)
	require.True(t, r.OK(), "problems: %v", r.Problems)
	require.Equal(t, 3, r.Transfers)
	require.Equal(t, 9, r.Postings)
	require.Equal(t, 4, r.Accounts)
}

// Postgres stores timestamptz to microsecond precision. A transfer posted with
// a finer timestamp must still verify, or the chain becomes unverifiable the
// moment it is read back — which is how this surfaced: green on macOS, where
// time.Now() is usually already microseconds, red on Linux.
func TestVerifySurvivesSubMicrosecondTimestamps(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	lg := ledger.New(pool)
	openBooks(t, lg)

	base := time.Date(2026, 5, 4, 10, 30, 0, 0, time.UTC)
	for i, ns := range []int{1, 499, 500, 999, 123456789} {
		_, err := lg.Post(ctx, &ledger.Transfer{
			Reference: fmt.Sprintf("ns-%d", i),
			PostedAt:  base.Add(time.Duration(ns)),
			Postings: (&ledger.Transfer{}).
				Debit("cash", try(100)).Credit("revenue", try(100)).Postings,
		})
		require.NoError(t, err)
	}

	// Now let the clock supply the precision instead of the test.
	_, err := lg.Post(ctx, (&ledger.Transfer{Reference: "ns-now"}).
		Debit("cash", try(100)).Credit("revenue", try(100)))
	require.NoError(t, err)

	r, err := lg.Verify(ctx)
	require.NoError(t, err)
	require.True(t, r.OK(), "problems: %v", r.Problems)
}

func TestVerifyCatchesTampering(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		tamper   string
		wantKind []string
	}{
		{
			name:     "cached balance edited",
			tamper:   `UPDATE account_balances SET balance = balance + 1 WHERE account_id = 'cash'`,
			wantKind: []string{"balance-drift", "trial-balance"},
		},
		{
			name:     "posting amount rewritten",
			tamper:   `UPDATE postings SET amount = amount + 500 WHERE account_id = 'cash'`,
			wantKind: []string{"unbalanced-transfer", "balance-drift", "hash-mismatch"},
		},
		{
			name:     "posting account swapped",
			tamper:   `UPDATE postings SET account_id = 'fees' WHERE account_id = 'cash' AND transfer_id = (SELECT min(id) FROM transfers)`,
			wantKind: []string{"balance-drift", "hash-mismatch"},
		},
		{
			name:     "transfer description edited",
			tamper:   `UPDATE transfers SET description = 'nothing to see here' WHERE id = (SELECT min(id) FROM transfers)`,
			wantKind: []string{"hash-mismatch"},
		},
		{
			name: "middle transfer deleted",
			tamper: `DELETE FROM postings WHERE transfer_id = (SELECT min(id) + 1 FROM transfers);
			         DELETE FROM transfers WHERE id = (SELECT min(id) + 1 FROM transfers)`,
			wantKind: []string{"chain-break", "balance-drift"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, lg := seedBooks(t)
			disableAppendOnly(t, pool)

			_, err := pool.Exec(ctx, tt.tamper)
			require.NoError(t, err)

			r, err := lg.Verify(ctx)
			require.NoError(t, err)
			require.False(t, r.OK(), "tampering went unnoticed")

			kinds := make(map[string]bool, len(r.Problems))
			for _, p := range r.Problems {
				kinds[p.Kind] = true
			}
			for _, want := range tt.wantKind {
				require.True(t, kinds[want], "expected a %q problem, got %v", want, r.Problems)
			}
		})
	}
}
