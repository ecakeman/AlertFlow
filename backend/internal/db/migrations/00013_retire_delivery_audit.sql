-- +goose Up
-- Audit rows must outlive the delivery they describe. A deleted integration
-- cascades away notification_deliveries, and the old foreign key took the
-- audit with it. delivery_id stays as a recorded identifier, not a live key.
-- Cancelled queued tasks also get a reason, copied onto the delivery row.

ALTER TABLE retired_delivery_audits
    ADD COLUMN event_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN integration_id BIGINT,
    ADD COLUMN integration_type TEXT NOT NULL DEFAULT '';

UPDATE retired_delivery_audits AS a
SET event_id = d.event_id,
    integration_id = d.integration_id,
    integration_type = i.type
FROM notification_deliveries AS d
JOIN integrations AS i ON i.id = d.integration_id
WHERE a.delivery_id = d.id;

ALTER TABLE retired_delivery_audits DROP CONSTRAINT IF EXISTS retired_delivery_audits_delivery_id_fkey;

-- +goose Down
DELETE FROM retired_delivery_audits AS a
WHERE NOT EXISTS (
    SELECT 1 FROM notification_deliveries AS d WHERE d.id = a.delivery_id
);
ALTER TABLE retired_delivery_audits
    ADD CONSTRAINT retired_delivery_audits_delivery_id_fkey
    FOREIGN KEY (delivery_id) REFERENCES notification_deliveries (id) ON DELETE CASCADE;
ALTER TABLE retired_delivery_audits
    DROP COLUMN integration_type,
    DROP COLUMN integration_id,
    DROP COLUMN event_id;
