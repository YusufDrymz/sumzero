package migrations_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/YusufDrymz/sumzero/migrations"
)

func TestApplyIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}
	ctx := context.Background()
	image := os.Getenv("SUMZERO_TEST_PG_IMAGE")
	if image == "" {
		image = "postgres:17-alpine"
	}
	ctr, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("sumzero"), tcpostgres.WithUsername("sumzero"), tcpostgres.WithPassword("sumzero"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(90*time.Second)))
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

	applied, err := migrations.Apply(ctx, pool)
	require.NoError(t, err)
	require.NotEmpty(t, applied)
	require.Equal(t, "0001_init.sql", applied[0], "file order is version order")

	again, err := migrations.Apply(ctx, pool)
	require.NoError(t, err)
	require.Empty(t, again, "a second run does nothing")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM sumzero_migrations`).Scan(&n))
	require.Equal(t, len(applied), n)
	for _, table := range []string{"accounts", "transfers", "postings", "account_balances", "idempotency_keys", "holds"} {
		var ok bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok))
		require.True(t, ok, table)
	}
}
