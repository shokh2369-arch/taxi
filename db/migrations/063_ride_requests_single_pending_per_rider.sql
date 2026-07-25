-- +goose Up
-- One PENDING ride request per rider, enforced at the database level.
--
-- The application checked this with a SELECT followed by an INSERT and no
-- transaction, so a double-tap or a client retry could interleave: both calls
-- saw no pending request and both inserted one. The rider then got dispatched
-- twice and a second driver drove to a ride that did not exist.
--
-- Production may already contain duplicates, so de-duplicate before adding the
-- index — otherwise this migration aborts and, because the container runs
-- `migrate -up && exec ./app`, the deploy crash-loops with no service at all.

-- Keep the most recent PENDING request per rider; retire the older ones.
UPDATE ride_requests
SET status = 'EXPIRED'
WHERE status = 'PENDING'
  AND rowid NOT IN (
    SELECT rowid FROM (
      SELECT rowid,
             ROW_NUMBER() OVER (
               PARTITION BY rider_user_id
               ORDER BY created_at DESC, rowid DESC
             ) AS rn
      FROM ride_requests
      WHERE status = 'PENDING'
    )
    WHERE rn = 1
  );

CREATE UNIQUE INDEX IF NOT EXISTS idx_ride_requests_one_pending_per_rider
ON ride_requests(rider_user_id) WHERE status = 'PENDING';

-- +goose Down
DROP INDEX IF EXISTS idx_ride_requests_one_pending_per_rider;
