-- +goose Up
-- Riders who blocked the bot cannot receive Telegram broadcasts; skip them in fan-out.
ALTER TABLE users ADD COLUMN telegram_bot_blocked INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- libSQL/SQLite 3.35+ supports DROP COLUMN
ALTER TABLE users DROP COLUMN telegram_bot_blocked;
