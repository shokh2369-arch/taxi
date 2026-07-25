-- +goose Up
-- Make uncollected commission a queryable number.
--
-- When a driver's promo and cash wallets cannot cover the commission, the
-- shortfall was written only into a metadata JSON blob on a ledger row whose
-- amount was 0, and nothing ever read it. The business could not answer "how
-- much are drivers into us for": summing driver_ledger returns wallet movement,
-- and summing payments of type 'commission' returns the promo that was clawed
-- back — which reads as revenue but is not cash.
--
-- These columns are additive. Existing amount semantics are unchanged: amount is
-- still what was actually taken from the wallets.
ALTER TABLE payments ADD COLUMN uncollected_amount INTEGER NOT NULL DEFAULT 0;
ALTER TABLE payments ADD COLUMN commission_due INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_payments_uncollected
  ON payments(driver_id) WHERE uncollected_amount > 0;

-- +goose Down
DROP INDEX IF EXISTS idx_payments_uncollected;
-- SQLite cannot drop columns on older versions; leaving them is harmless.
SELECT 1;
