-- +goose Up
CREATE TABLE system.episode_fault_document_refs (
    episode_id UUID NOT NULL REFERENCES system.episodes(episode_id),
    fd_id UUID NOT NULL,
    task_id UUID NOT NULL REFERENCES system.agent_tasks(task_id),
    execution_generation BIGINT NOT NULL,
    content_hash CHAR(64) NOT NULL,
    registered_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (task_id, execution_generation)
);

-- +goose Down
DROP TABLE IF EXISTS system.episode_fault_document_refs;
