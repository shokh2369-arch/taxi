-- +goose Up
-- Enforce unique full plate number for drivers.
-- Plate is normalized to uppercase in the bot; this index prevents duplicates at DB level.
--
-- NOTE: Production DB may already contain duplicates. To avoid deploy failures,
-- we keep the earliest driver record (min(user_id)) for each duplicated plate,
-- and force the other duplicates to re-enter their plate by clearing plate fields.
-- This preserves the constraint without deleting data.

-- Clear duplicate plate_number values (keep min(user_id) for each plate_number).
UPDATE drivers
SET plate_number = NULL,
    plate = NULL,
    application_step = 'plate',
    verification_status = 'rejected',
    is_active = 0
WHERE user_id IN (
  SELECT d.user_id
  FROM drivers d
  JOIN (
    SELECT plate_number
    FROM drivers
    WHERE plate_number IS NOT NULL AND plate_number != ''
    GROUP BY plate_number
    HAVING COUNT(*) > 1
  ) dup ON dup.plate_number = d.plate_number
  WHERE d.user_id != (
    SELECT MIN(user_id) FROM drivers d2 WHERE d2.plate_number = dup.plate_number
  )
);

-- Clear duplicate plate values (keep min(user_id) for each plate).
UPDATE drivers
SET plate_number = NULL,
    plate = NULL,
    application_step = 'plate',
    verification_status = 'rejected',
    is_active = 0
WHERE user_id IN (
  SELECT d.user_id
  FROM drivers d
  JOIN (
    SELECT plate
    FROM drivers
    WHERE plate IS NOT NULL AND plate != ''
    GROUP BY plate
    HAVING COUNT(*) > 1
  ) dup ON dup.plate = d.plate
  WHERE d.user_id != (
    SELECT MIN(user_id) FROM drivers d2 WHERE d2.plate = dup.plate
  )
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_drivers_plate_number_unique ON drivers(plate_number);
CREATE UNIQUE INDEX IF NOT EXISTS idx_drivers_plate_unique ON drivers(plate);

-- +goose Down
DROP INDEX IF EXISTS idx_drivers_plate_unique;
DROP INDEX IF EXISTS idx_drivers_plate_number_unique;

