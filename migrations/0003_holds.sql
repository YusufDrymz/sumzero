-- Holds and the overdraft guard.
--
-- A hold reserves an amount on an account without moving it: the card was
-- authorised, the payout is queued, the withdrawal is pending review. Money
-- moves only on capture, which posts an ordinary transfer and closes the hold
-- in the same transaction.
--
-- Holds are only meaningful together with the guard: an account that may not
-- go negative counts its active holds as already spent.

BEGIN;

-- Default true keeps v0.1 behaviour: the ledger records, it does not police.
-- Set false on accounts that represent a real balance someone can overdraw.
ALTER TABLE accounts ADD COLUMN allow_negative boolean NOT NULL DEFAULT true;

CREATE TABLE holds (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id  text    NOT NULL REFERENCES accounts (id),
    reference   text    NOT NULL,
    amount      bigint  NOT NULL CHECK (amount > 0),
    currency    char(3) NOT NULL,
    status      text    NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'captured', 'released', 'expired')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    closed_at   timestamptz,

    -- Set on capture: the transfer that actually moved the money.
    transfer_id bigint REFERENCES transfers (id)
);

CREATE UNIQUE INDEX holds_reference_key ON holds (reference);

-- The available-balance query reads active holds per account; everything else
-- is by reference.
CREATE INDEX holds_active_idx ON holds (account_id) WHERE status = 'active';
CREATE INDEX holds_expiry_idx ON holds (expires_at) WHERE status = 'active';

COMMIT;
