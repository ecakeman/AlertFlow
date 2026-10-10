-- +goose Up
-- Pending delivery tasks cannot be sent after their channel is gone.
-- Deleting them is not a record that the notification was sent.
ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_integration_id_fkey;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_integration_id_fkey
    FOREIGN KEY (integration_id) REFERENCES integrations (id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_integration_id_fkey;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_integration_id_fkey
    FOREIGN KEY (integration_id) REFERENCES integrations (id);
