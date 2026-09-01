package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	idempotent "github.com/YusufDrymz/go-idempotent"
)

// IdempotencyStore is a Postgres-backed idempotent.Store.
//
// Keys live in the same database as the ledger, so a retry is judged against
// the same state the transfer was written to — with an in-memory store, two
// API instances would each think they were the first.
type IdempotencyStore struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

// NewIdempotencyStore returns a store that keeps completed responses for ttl.
func NewIdempotencyStore(pool *pgxpool.Pool, ttl time.Duration) *IdempotencyStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &IdempotencyStore{pool: pool, ttl: ttl}
}

// Begin reserves a key. The insert itself is the lock: whoever wins the primary
// key owns the request, and everyone else is told what happened to it.
//
// Re-reserving an expired key also clears the old response. Otherwise a handler
// that fails on the retry would Release nothing (the row looks completed) and
// the next attempt would replay a stale answer.
func (s *IdempotencyStore) Begin(ctx context.Context, key string) (idempotent.BeginResult, *idempotent.Entry, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (key, expires_at)
		VALUES ($1, now() + make_interval(secs => $2))
		ON CONFLICT (key) DO UPDATE
		SET expires_at = excluded.expires_at,
		    fingerprint = NULL, status_code = NULL, headers = NULL, body = NULL,
		    created_at = now()
		WHERE idempotency_keys.expires_at <= now()`,
		key, s.ttl.Seconds())
	if err != nil {
		return 0, nil, err
	}
	if tag.RowsAffected() == 1 {
		return idempotent.Begun, nil, nil
	}

	var (
		fingerprint *string
		status      *int
		headers     []byte
		body        []byte
	)
	err = s.pool.QueryRow(ctx, `
		SELECT fingerprint, status_code, headers, body FROM idempotency_keys WHERE key = $1`, key).
		Scan(&fingerprint, &status, &headers, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		// The row expired between the insert and this read. Treat it as a live
		// reservation rather than replaying nothing.
		return idempotent.InFlight, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	if status == nil {
		return idempotent.InFlight, nil, nil
	}

	entry := idempotent.Entry{StatusCode: *status, Body: body, Header: http.Header{}}
	if fingerprint != nil {
		entry.Fingerprint = *fingerprint
	}
	if len(headers) > 0 {
		if err := json.Unmarshal(headers, &entry.Header); err != nil {
			return 0, nil, err
		}
	}
	return idempotent.Completed, &entry, nil
}

// Complete stores the response so a retry replays it.
func (s *IdempotencyStore) Complete(ctx context.Context, key string, entry idempotent.Entry) error {
	headers, err := json.Marshal(entry.Header)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET fingerprint = $2, status_code = $3, headers = $4, body = $5
		WHERE key = $1`, key, entry.Fingerprint, entry.StatusCode, headers, entry.Body)
	return err
}

// Release frees a reservation whose handler never produced a response.
func (s *IdempotencyStore) Release(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE key = $1 AND status_code IS NULL`, key)
	return err
}

// Sweep deletes expired keys. Call it from a cron or a ticker; nothing breaks if
// it never runs, the table just grows.
func (s *IdempotencyStore) Sweep(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
