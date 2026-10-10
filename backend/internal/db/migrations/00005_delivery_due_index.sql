-- +goose Up
-- Claim looks up due pending/retry_wait rows and expired running leases.
-- The pending-only index from 00003 does not cover those states.

CREATE INDEX idx_notification_deliveries_due
    ON notification_deliveries (next_attempt_at, id)
    WHERE status IN ('pending', 'retry_wait');

CREATE INDEX idx_notification_deliveries_running
    ON notification_deliveries (lease_until, id)
    WHERE status = 'running';

-- +goose Down
DROP INDEX IF EXISTS idx_notification_deliveries_running;
DROP INDEX IF EXISTS idx_notification_deliveries_due;
