-- Idempotency keys for the HTTP API.
--
-- The transfers table already refuses a duplicate reference, so the ledger
-- cannot double-post either way. This table answers the other half of the
-- question: a retried request must get the *same response* back, not a 409 it
-- has to interpret.

BEGIN;

CREATE TABLE idempotency_keys (
    key         text PRIMARY KEY,

    -- Digest of the original request body. A key reused with different content
    -- is a client bug, and returning the first response would hide it.
    fingerprint text,

    -- NULL until the handler finishes. A row with a NULL status is a request
    -- in flight, which is how a concurrent retry is detected.
    status_code int,
    headers     jsonb,
    body        bytea,

    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);

CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

COMMIT;
