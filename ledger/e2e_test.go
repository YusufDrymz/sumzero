package ledger_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/YusufDrymz/sumzero/ledger"
)

func try(minor int64) ledger.Money { return ledger.Amount(minor, "TRY") }

func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}
	ctx := context.Background()

	image := os.Getenv("SUMZERO_TEST_PG_IMAGE")
	if image == "" {
		image = "postgres:17-alpine"
	}

	ctr, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("sumzero"),
		tcpostgres.WithUsername("sumzero"),
		tcpostgres.WithPassword("sumzero"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("e2e: container start failed in CI: %v", err)
		}
		t.Skipf("e2e: docker unavailable, skipping: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema, err := os.ReadFile(filepath.Join("..", "migrations", "0001_init.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(schema))
	require.NoError(t, err)

	return pool
}

// openBooks sets up a small chart of accounts used by most tests.
func openBooks(t *testing.T, lg *ledger.Ledger) {
	t.Helper()
	ctx := context.Background()
	for _, a := range []ledger.Account{
		{ID: "cash", Type: ledger.Asset, Currency: "TRY"},
		{ID: "fees", Type: ledger.Expense, Currency: "TRY"},
		{ID: "revenue", Type: ledger.Income, Currency: "TRY"},
		{ID: "payable", Type: ledger.Liability, Currency: "TRY"},
	} {
		require.NoError(t, lg.CreateAccount(ctx, a))
	}
}

func TestPostAndBalance(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)

	// A 100.00 TRY sale where 10.00 is commission.
	_, err := lg.Post(ctx, (&ledger.Transfer{Reference: "order-1", Description: "sale"}).
		Debit("cash", try(9000)).
		Debit("fees", try(1000)).
		Credit("revenue", try(10000)))
	require.NoError(t, err)

	// Balances are reported on each account's normal side: the asset and the
	// expense are debit-normal, the income account is credit-normal, and all
	// three read positive.
	for _, want := range []struct {
		account string
		amount  int64
	}{
		{"cash", 9000},
		{"fees", 1000},
		{"revenue", 10000},
	} {
		bal, err := lg.Balance(ctx, want.account)
		require.NoError(t, err)
		require.Equal(t, ledger.Amount(want.amount, "TRY"), bal, want.account)
	}

	tb, err := lg.TrialBalance(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), tb["TRY"], "books must balance")
}

func TestPostRejections(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)
	require.NoError(t, lg.CreateAccount(ctx, ledger.Account{ID: "usd_cash", Type: ledger.Asset, Currency: "USD"}))
	require.NoError(t, lg.CreateAccount(ctx, ledger.Account{ID: "old", Type: ledger.Asset, Currency: "TRY"}))
	require.NoError(t, lg.Archive(ctx, "old"))

	_, err := lg.Post(ctx, (&ledger.Transfer{Reference: "dup"}).
		Debit("cash", try(100)).Credit("revenue", try(100)))
	require.NoError(t, err)

	tests := []struct {
		name string
		tx   *ledger.Transfer
		want error
	}{
		{
			name: "same reference twice",
			tx:   (&ledger.Transfer{Reference: "dup"}).Debit("cash", try(500)).Credit("revenue", try(500)),
			want: ledger.ErrDuplicateReference,
		},
		{
			name: "unknown account",
			tx:   (&ledger.Transfer{Reference: "r1"}).Debit("ghost", try(100)).Credit("revenue", try(100)),
			want: ledger.ErrUnknownAccount,
		},
		{
			name: "archived account",
			tx:   (&ledger.Transfer{Reference: "r2"}).Debit("old", try(100)).Credit("revenue", try(100)),
			want: ledger.ErrAccountArchived,
		},
		{
			name: "posting currency does not match account",
			tx:   (&ledger.Transfer{Reference: "r3"}).Debit("usd_cash", try(100)).Credit("revenue", try(100)),
			want: ledger.ErrCurrencyMismatch,
		},
		{
			name: "unbalanced",
			tx:   (&ledger.Transfer{Reference: "r4"}).Debit("cash", try(100)).Credit("revenue", try(99)),
			want: ledger.ErrUnbalanced,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lg.Post(ctx, tt.tx)
			require.ErrorIs(t, err, tt.want)
		})
	}

	// Every rejection above must have left the books untouched.
	bal, err := lg.Balance(ctx, "cash")
	require.NoError(t, err)
	require.Equal(t, try(100), bal)
}

func TestBalanceAsOfFollowsLedgerDateNotWriteOrder(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)

	day := func(d int) time.Time {
		return time.Date(2026, 3, d, 12, 0, 0, 0, time.UTC)
	}

	_, err := lg.Post(ctx, &ledger.Transfer{Reference: "mar-10", PostedAt: day(10),
		Postings: (&ledger.Transfer{}).Debit("cash", try(5000)).Credit("revenue", try(5000)).Postings})
	require.NoError(t, err)

	// Written second, dated first: a late-arriving invoice. A ledger that
	// ordered by insertion would report the wrong balance for March 5th.
	_, err = lg.Post(ctx, &ledger.Transfer{Reference: "mar-05", PostedAt: day(5),
		Postings: (&ledger.Transfer{}).Debit("cash", try(2000)).Credit("revenue", try(2000)).Postings})
	require.NoError(t, err)

	for _, want := range []struct {
		at      time.Time
		balance int64
	}{
		{day(1), 0},
		{day(5), 2000},
		{day(7), 2000},
		{day(10), 7000},
		{day(30), 7000},
	} {
		bal, err := lg.BalanceAsOf(ctx, "cash", want.at)
		require.NoError(t, err)
		require.Equal(t, want.balance, bal.Amount, "as of %s", want.at.Format("2006-01-02"))
	}

	// The cached balance and the recomputed one must agree at the end.
	current, err := lg.Balance(ctx, "cash")
	require.NoError(t, err)
	require.Equal(t, int64(7000), current.Amount)
}

func TestStatementOrderAndWindow(t *testing.T) {
	ctx := context.Background()
	lg := ledger.New(startPostgres(t))
	openBooks(t, lg)

	for i := 1; i <= 5; i++ {
		_, err := lg.Post(ctx, &ledger.Transfer{
			Reference: "tx-" + string(rune('0'+i)),
			PostedAt:  time.Date(2026, 4, i, 9, 0, 0, 0, time.UTC),
			Postings: (&ledger.Transfer{}).
				Debit("cash", try(int64(i)*100)).Credit("revenue", try(int64(i)*100)).Postings,
		})
		require.NoError(t, err)
	}

	all, err := lg.Statement(ctx, "cash", ledger.StatementOptions{})
	require.NoError(t, err)
	require.Len(t, all, 5)
	require.Equal(t, "tx-5", all[0].Reference, "newest first")
	require.Equal(t, "tx-1", all[4].Reference)

	window, err := lg.Statement(ctx, "cash", ledger.StatementOptions{
		From: time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 4, 4, 23, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.Len(t, window, 3)
	require.Equal(t, ledger.Debit, window[0].Dir)
	require.Equal(t, try(400), window[0].Amount)
}

// The append-only rule has to hold against direct SQL, not just against this
// package. Otherwise it is a convention, and conventions do not survive 2am.
func TestHistoryCannotBeRewrittenWithSQL(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	lg := ledger.New(pool)
	openBooks(t, lg)

	id, err := lg.Post(ctx, (&ledger.Transfer{Reference: "immutable"}).
		Debit("cash", try(10000)).Credit("revenue", try(10000)))
	require.NoError(t, err)

	for _, stmt := range []struct {
		name string
		sql  string
	}{
		{"update posting amount", `UPDATE postings SET amount = 1 WHERE transfer_id = $1`},
		{"delete postings", `DELETE FROM postings WHERE transfer_id = $1`},
		{"update transfer", `UPDATE transfers SET description = 'edited' WHERE id = $1`},
		{"delete transfer", `DELETE FROM transfers WHERE id = $1`},
	} {
		t.Run(stmt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, stmt.sql, id)
			require.Error(t, err, "the database must refuse this")
			require.Contains(t, err.Error(), "not allowed")
		})
	}
}

// The hash chain is only worth having if each link actually covers the one
// before it.
func TestHashChainLinksEveryTransfer(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	lg := ledger.New(pool)
	openBooks(t, lg)

	for i := 1; i <= 3; i++ {
		_, err := lg.Post(ctx, (&ledger.Transfer{Reference: "chain-" + string(rune('0'+i))}).
			Debit("cash", try(100)).Credit("revenue", try(100)))
		require.NoError(t, err)
	}

	rows, err := pool.Query(ctx, `SELECT prev_hash, hash FROM transfers ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	var prev []byte
	var n int
	for rows.Next() {
		var gotPrev, hash []byte
		require.NoError(t, rows.Scan(&gotPrev, &hash))
		require.Equal(t, prev, gotPrev, "transfer %d does not point at its predecessor", n)
		require.NotEmpty(t, hash)
		prev, n = hash, n+1
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 3, n)
}

// The embedded mode: the ledger joins the caller's transaction, so the entry
// and whatever it describes commit or roll back together.
func TestEmbeddedModeSharesCallerTransaction(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	openBooks(t, ledger.New(pool))

	_, err := pool.Exec(ctx, `CREATE TABLE orders (id text PRIMARY KEY, state text NOT NULL)`)
	require.NoError(t, err)

	t.Run("rollback leaves no trace", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)

		_, err = tx.Exec(ctx, `INSERT INTO orders (id, state) VALUES ('o-1', 'paid')`)
		require.NoError(t, err)
		_, err = ledger.NewTx(tx).Post(ctx, (&ledger.Transfer{Reference: "o-1"}).
			Debit("cash", try(2500)).Credit("revenue", try(2500)))
		require.NoError(t, err)

		require.NoError(t, tx.Rollback(ctx)) // the caller's own write failed later

		bal, err := ledger.New(pool).Balance(ctx, "cash")
		require.NoError(t, err)
		require.Equal(t, int64(0), bal.Amount, "rolled-back transfer must not be posted")
	})

	t.Run("commit lands both", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)

		_, err = tx.Exec(ctx, `INSERT INTO orders (id, state) VALUES ('o-2', 'paid')`)
		require.NoError(t, err)
		_, err = ledger.NewTx(tx).Post(ctx, (&ledger.Transfer{Reference: "o-2"}).
			Debit("cash", try(2500)).Credit("revenue", try(2500)))
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))

		bal, err := ledger.New(pool).Balance(ctx, "cash")
		require.NoError(t, err)
		require.Equal(t, int64(2500), bal.Amount)
	})
}

// Concurrent retries of the same payment must post exactly once, and a burst of
// distinct transfers must leave the books balanced.
func TestConcurrentPosts(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	lg := ledger.New(pool)
	openBooks(t, lg)

	t.Run("same reference races to one row", func(t *testing.T) {
		const attempts = 8
		var wg sync.WaitGroup
		errs := make([]error, attempts)

		for i := 0; i < attempts; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = lg.Post(ctx, (&ledger.Transfer{Reference: "retry-me"}).
					Debit("cash", try(1000)).Credit("revenue", try(1000)))
			}(i)
		}
		wg.Wait()

		var ok, dup int
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ledger.ErrDuplicateReference):
				dup++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		require.Equal(t, 1, ok, "exactly one attempt may win")
		require.Equal(t, attempts-1, dup)

		bal, err := lg.Balance(ctx, "cash")
		require.NoError(t, err)
		require.Equal(t, int64(1000), bal.Amount)
	})

	t.Run("burst keeps the books balanced", func(t *testing.T) {
		const n = 25
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := lg.Post(ctx, (&ledger.Transfer{Reference: "burst-" + string(rune('a'+i))}).
					Debit("cash", try(300)).
					Credit("payable", try(200)).
					Credit("revenue", try(100)))
				require.NoError(t, err)
			}(i)
		}
		wg.Wait()

		tb, err := lg.TrialBalance(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(0), tb["TRY"])

		// And the chain has no gaps despite the concurrency.
		var chained int
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM transfers a JOIN transfers b ON b.id = a.id - 1
			WHERE a.prev_hash IS DISTINCT FROM b.hash`).Scan(&chained))
		require.Equal(t, 0, chained, "every link must point at the previous transfer")
	})
}
