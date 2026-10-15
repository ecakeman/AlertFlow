-- +goose Up
-- Two CHECK constraints predate the agent edges and would reject their rows:
--   * integrations.type allowed only the notification channels, so an
--     agent_webhook channel could not be created at all
--   * notification_log.event allowed only the three notification events, so
--     logging a chat delivery would fail
--
-- Both are widened rather than dropped: the point of the constraint is to keep
-- rejecting typos, not to stop constraining the column.

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_type_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_type_check
    CHECK (type IN ('telegram', 'slack', 'email', 'webhook', 'agent_webhook'));

ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_check;
ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_check
    CHECK (event IN ('incident_start', 'incident_resolve', 'warning', 'chat_requested'));

-- +goose Down
ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_check;
ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_check
    CHECK (event IN ('incident_start', 'incident_resolve', 'warning'));

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_type_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_type_check
    CHECK (type IN ('telegram', 'slack', 'email', 'webhook'));
