-- +goose Up
CREATE TABLE a2a_adapter_tasks (
    message_id text PRIMARY KEY,
    task_id text NOT NULL,
    status text NOT NULL,
    artifact jsonb,
    task_json jsonb,
    deadline_at timestamptz,
    cancelled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX a2a_adapter_tasks_task_id ON a2a_adapter_tasks (task_id);

-- +goose Down
DROP TABLE a2a_adapter_tasks;
