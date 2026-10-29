-- +goose Up
CREATE TABLE a2a_adapter_task_aliases (
    task_id text PRIMARY KEY,
    message_id text NOT NULL REFERENCES a2a_adapter_tasks (message_id)
);

-- +goose Down
DROP TABLE a2a_adapter_task_aliases;
