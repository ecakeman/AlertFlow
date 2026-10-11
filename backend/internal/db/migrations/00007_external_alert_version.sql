-- +goose Up
-- last_occurred_at is the newest event time applied to this alert. Events at
-- the same second do not have a recoverable order, so they must not replace it.
ALTER TABLE external_alerts ADD COLUMN last_occurred_at BIGINT NOT NULL DEFAULT 0;
UPDATE external_alerts SET last_occurred_at = GREATEST(opened_at, COALESCE(resolved_at, 0));

-- +goose Down
ALTER TABLE external_alerts DROP COLUMN last_occurred_at;
