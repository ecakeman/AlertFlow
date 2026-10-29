-- +goose Up
-- Internal Agent tasks are delivery rows with no integration. Historical
-- external rows keep their integration_id and are not deleted.

ALTER TABLE notification_deliveries
    ALTER COLUMN integration_id DROP NOT NULL;

CREATE UNIQUE INDEX notification_deliveries_internal_event
    ON notification_deliveries (event_id, event_type)
    WHERE integration_id IS NULL;

-- +goose Down
DROP INDEX IF EXISTS notification_deliveries_internal_event;
ALTER TABLE notification_deliveries
    ALTER COLUMN integration_id SET NOT NULL;
