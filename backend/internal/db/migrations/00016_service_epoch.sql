-- +goose Up
-- Epoch distinguishes a service life from the one that was deleted, including
-- when both happen in the same Unix second. Events without an epoch still use
-- retired_at.

CREATE TABLE service_epochs (
    service_id TEXT PRIMARY KEY,
    epoch      BIGINT NOT NULL
);

ALTER TABLE retired_services
    ADD COLUMN epoch BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE retired_services DROP COLUMN IF EXISTS epoch;
DROP TABLE IF EXISTS service_epochs;
