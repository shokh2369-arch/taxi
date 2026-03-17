-- +goose Up
-- Enforce unique full plate number for drivers.
-- Plate is normalized to uppercase in the bot; this index prevents duplicates at DB level.
CREATE UNIQUE INDEX IF NOT EXISTS idx_drivers_plate_number_unique ON drivers(plate_number);
CREATE UNIQUE INDEX IF NOT EXISTS idx_drivers_plate_unique ON drivers(plate);

-- +goose Down
DROP INDEX IF EXISTS idx_drivers_plate_unique;
DROP INDEX IF EXISTS idx_drivers_plate_number_unique;

