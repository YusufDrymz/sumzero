package ledger_test

import (
	"context"
	"testing"
	"time"

	idempotent "github.com/YusufDrymz/go-idempotent"
	"github.com/stretchr/testify/require"

	"github.com/YusufDrymz/sumzero/ledger"
)

func TestIdempotencyStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	store := ledger.NewIdempotencyStore(startPostgres(t), time.Hour)

	res, _, err := store.Begin(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, idempotent.Begun, res)

	res, _, err = store.Begin(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, idempotent.InFlight, res, "a reserved key is busy until completed")

	require.NoError(t, store.Complete(ctx, "k", idempotent.Entry{StatusCode: 201, Body: []byte("first"), Fingerprint: "fp"}))
	res, entry, err := store.Begin(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, idempotent.Completed, res)
	require.Equal(t, "first", string(entry.Body))

	// Release must not touch a completed key.
	require.NoError(t, store.Release(ctx, "k"))
	res, _, err = store.Begin(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, idempotent.Completed, res)
}

// An expired key that is reserved again must not carry the old response along.
// If it did, a failing retry would Release nothing (the row still looks
// completed) and the attempt after that would replay a stale answer.
func TestIdempotencyStoreExpiryClearsOldResponse(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	store := ledger.NewIdempotencyStore(pool, time.Hour)

	res, _, err := store.Begin(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, idempotent.Begun, res)
	require.NoError(t, store.Complete(ctx, "k", idempotent.Entry{StatusCode: 201, Body: []byte("stale")}))

	_, err = pool.Exec(ctx, `UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE key = 'k'`)
	require.NoError(t, err)

	res, _, err = store.Begin(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, idempotent.Begun, res, "an expired key is free again")

	// The handler fails this time and releases the key.
	require.NoError(t, store.Release(ctx, "k"))

	res, _, err = store.Begin(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, idempotent.Begun, res, "released key must be free, not replaying the stale body")
}

func TestIdempotencyStoreSweep(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	store := ledger.NewIdempotencyStore(pool, time.Hour)

	for _, k := range []string{"a", "b", "c"} {
		_, _, err := store.Begin(ctx, k)
		require.NoError(t, err)
	}
	_, err := pool.Exec(ctx, `UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE key IN ('a', 'b')`)
	require.NoError(t, err)

	n, err := store.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
}
