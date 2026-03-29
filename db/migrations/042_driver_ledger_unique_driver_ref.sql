-- +goose Up
-- Strict idempotency: one ledger row per (driver_id, reference_type, reference_id). Enables INSERT OR IGNORE for promo/referral.
DROP INDEX IF EXISTS idx_driver_ledger_first3_trip;
DROP INDEX IF EXISTS idx_driver_ledger_referral_reward;

-- Legacy commission wrote multiple rows as reference_type='trip' with the same reference_id (trip id).
-- Match app semantics (wallet.ApplyTripCommissionInTx): one stable key per entry_type.
UPDATE driver_ledger
SET reference_id = reference_id || ':' || entry_type
WHERE reference_type = 'trip'
  AND entry_type IN ('COMMISSION_ACCRUED', 'PROMO_APPLIED_TO_COMMISSION', 'CASH_APPLIED_TO_COMMISSION')
  AND reference_id IS NOT NULL
  AND TRIM(reference_id) != ''
  AND reference_id NOT LIKE ('%:' || entry_type);

-- Any remaining duplicate triples (e.g. accidental duplicates): keep smallest id, uniquify others.
UPDATE driver_ledger AS d1
SET reference_id = TRIM(d1.reference_id) || ':ledger:' || d1.id
WHERE d1.reference_type IS NOT NULL
  AND d1.reference_id IS NOT NULL
  AND LENGTH(TRIM(d1.reference_id)) > 0
  AND EXISTS (
    SELECT 1 FROM driver_ledger AS d2
    WHERE d2.driver_id = d1.driver_id
      AND d2.reference_type = d1.reference_type
      AND d2.reference_id = d1.reference_id
      AND d2.id < d1.id
  );

CREATE UNIQUE INDEX IF NOT EXISTS idx_driver_ledger_driver_ref_type_id
ON driver_ledger(driver_id, reference_type, reference_id);

-- +goose Down
DROP INDEX IF EXISTS idx_driver_ledger_driver_ref_type_id;

CREATE UNIQUE INDEX IF NOT EXISTS idx_driver_ledger_first3_trip
ON driver_ledger(driver_id, reference_id)
WHERE reference_type = 'first_3_trip_bonus' AND entry_type = 'PROMO_GRANTED';

CREATE UNIQUE INDEX IF NOT EXISTS idx_driver_ledger_referral_reward
ON driver_ledger(driver_id, reference_id)
WHERE reference_type = 'referral_reward' AND entry_type = 'PROMO_GRANTED';
