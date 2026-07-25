-- +goose Up
-- One-time reset of manual_offline, to make the semantics change safe to deploy.
--
-- manual_offline used to be cleared by ANY driver location ping, so a driver who
-- toggled OFFLINE was silently put back online by the next background update —
-- which is the bug being fixed. Now only POST /driver/online (new) or the
-- Telegram live-location paths clear it.
--
-- That leaves a migration hazard: every driver currently sitting at
-- manual_offline = 1 was going to be cleared by their next ping, and after this
-- deploy will not be. On an app build that predates POST /driver/online, those
-- drivers are stranded offline with no in-app way back, and the symptom is
-- "I get no orders" with nothing in the logs.
--
-- Clearing the flag once starts the explicit model from a clean slate. Nobody is
-- wrongly put online by this: dispatch still independently requires a fresh
-- location, positive balance, approval and legal acceptance, and a driver who is
-- genuinely off shift simply stops pinging and ages out of the freshness window.
UPDATE drivers SET manual_offline = 0 WHERE COALESCE(manual_offline, 0) = 1;

-- +goose Down
-- Nothing to restore: the previous per-driver values were not durable state,
-- they were about to be cleared by the next location ping anyway.
SELECT 1;
