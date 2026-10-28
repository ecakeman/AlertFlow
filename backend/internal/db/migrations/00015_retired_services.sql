-- +goose Up
-- A deleted service stays recognizable after its outbox rows are gone.
-- Publisher and consumer both read this row, so an event already loaded into
-- memory cannot recreate incidents or runtime state.

CREATE TABLE retired_services (
    service_id TEXT PRIMARY KEY,
    retired_at BIGINT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS retired_services;
