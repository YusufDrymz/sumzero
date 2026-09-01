-- Reversals as a first-class link.
--
-- A correction has always been "post the opposite transfer" (ADR-0002). This
-- makes the relationship explicit so a statement can show what reversed what,
-- and so the same transfer cannot be reversed twice.

BEGIN;

ALTER TABLE transfers ADD COLUMN reverses_transfer_id bigint REFERENCES transfers (id);

-- One reversal per transfer. Reversing a reversal is allowed — it is a new
-- transfer pointing at the reversal — but the original stays reversed once.
CREATE UNIQUE INDEX transfers_reverses_key ON transfers (reverses_transfer_id)
    WHERE reverses_transfer_id IS NOT NULL;

COMMIT;
