-- sumzero core schema.
--
-- Two rules shape everything here:
--   1. Postings are immutable. No UPDATE, no DELETE. A correction is a new
--      reversing transfer. This is what makes an as-of balance trustworthy.
--   2. Every transfer nets to zero per currency. The application enforces it;
--      `sumzero verify` re-proves it from the rows.
--
-- The schema is public and documented on purpose: your data stays queryable
-- with plain SQL, with or without this tool.

BEGIN;

CREATE TABLE accounts (
    id         text PRIMARY KEY,
    type       text        NOT NULL CHECK (type IN ('asset','liability','equity','income','expense')),
    currency   char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    archived   boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE transfers (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    reference   text        NOT NULL,
    description text        NOT NULL DEFAULT '',

    -- Ledger date. May be backdated, so as-of queries order by this column,
    -- never by id.
    posted_at   timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    -- Tamper-evident chain: each row carries the digest of the previous one.
    -- Deleting or rewriting history breaks the chain and `verify` says where.
    prev_hash   bytea,
    hash        bytea       NOT NULL
);

-- One reference is one movement. The unique index is the last line of defence
-- behind the idempotency key: a retried request cannot post twice even if the
-- key cache is cold.
CREATE UNIQUE INDEX transfers_reference_key ON transfers (reference);
CREATE INDEX transfers_posted_at_idx ON transfers (posted_at, id);

CREATE TABLE postings (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transfer_id bigint  NOT NULL REFERENCES transfers (id),
    account_id  text    NOT NULL REFERENCES accounts (id),

    -- Always positive. The side lives in `direction`, so there is exactly one
    -- way to express a movement.
    amount      bigint  NOT NULL CHECK (amount > 0),
    currency    char(3) NOT NULL,
    direction   text    NOT NULL CHECK (direction IN ('debit','credit'))
);

-- Statements and as-of balances both read postings by account in ledger-date
-- order; the transfer_id tiebreaker keeps paging stable when timestamps tie.
CREATE INDEX postings_account_idx ON postings (account_id, transfer_id);
CREATE INDEX postings_transfer_idx ON postings (transfer_id);

-- Materialised current balance. It is a cache, not the truth: the truth is the
-- sum of postings, and `sumzero verify` recomputes it to catch any drift.
CREATE TABLE account_balances (
    account_id text PRIMARY KEY REFERENCES accounts (id),

    -- Debit-positive minor units. Reading code applies the account's normal
    -- side, so nothing here depends on presentation.
    balance    bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- History is append-only at the database level too, so a stray UPDATE in some
-- future migration or a curious psql session fails loudly instead of quietly
-- rewriting someone's balance.
CREATE FUNCTION sumzero_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'sumzero: % on % is not allowed; post a reversing transfer instead',
        TG_OP, TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER transfers_append_only
    BEFORE UPDATE OR DELETE ON transfers
    FOR EACH ROW EXECUTE FUNCTION sumzero_append_only();

CREATE TRIGGER postings_append_only
    BEFORE UPDATE OR DELETE ON postings
    FOR EACH ROW EXECUTE FUNCTION sumzero_append_only();

COMMIT;
