-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE SCHEMA IF NOT EXISTS system;
CREATE SCHEMA IF NOT EXISTS cognitive;

CREATE TABLE system.services (
    service_id UUID PRIMARY KEY,
    service_key VARCHAR(100) UNIQUE NOT NULL,
    name VARCHAR(200) NOT NULL,
    description TEXT,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    monitor_aggregation_mode VARCHAR(30) NOT NULL,
    authority_producer_id VARCHAR(100) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT services_aggregation_chk CHECK (monitor_aggregation_mode = 'ANY_REQUIRED')
);

CREATE TABLE system.service_monitor_targets (
    target_id UUID PRIMARY KEY,
    service_id UUID NOT NULL REFERENCES system.services(service_id),
    target_key VARCHAR(100) NOT NULL,
    target_name VARCHAR(200) NOT NULL,
    target_type VARCHAR(30) NOT NULL,
    address TEXT NOT NULL,
    config JSONB NOT NULL DEFAULT '{}',
    interval_ms INTEGER NOT NULL,
    timeout_ms INTEGER NOT NULL,
    failure_threshold INTEGER NOT NULL,
    recovery_threshold INTEGER NOT NULL,
    is_required BOOLEAN NOT NULL DEFAULT TRUE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT targets_type_chk CHECK (target_type IN ('HTTP', 'TCP')),
    CONSTRAINT targets_threshold_chk CHECK (failure_threshold > 0 AND recovery_threshold > 0),
    UNIQUE (service_id, target_key)
);

CREATE TABLE system.service_monitor_target_states (
    target_id UUID PRIMARY KEY REFERENCES system.service_monitor_targets(target_id),
    status VARCHAR(20) NOT NULL,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    consecutive_successes INTEGER NOT NULL DEFAULT 0,
    last_check_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    last_failure_at TIMESTAMPTZ,
    last_point_seq BIGINT,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT target_status_chk CHECK (status IN ('UNKNOWN', 'NORMAL', 'ABNORMAL'))
);

CREATE TABLE system.service_source_states (
    service_id UUID NOT NULL REFERENCES system.services(service_id),
    source VARCHAR(20) NOT NULL,
    status VARCHAR(20) NOT NULL,
    last_event_id VARCHAR(128),
    last_event_time TIMESTAMPTZ,
    last_received_at TIMESTAMPTZ,
    last_source_revision BIGINT,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (service_id, source),
    CONSTRAINT source_kind_chk CHECK (source IN ('MONITOR', 'APP')),
    CONSTRAINT source_status_chk CHECK (status IN ('UNKNOWN', 'NORMAL', 'ABNORMAL'))
);

CREATE TABLE system.service_monitor_points (
    point_seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    point_id UUID UNIQUE NOT NULL,
    service_id UUID NOT NULL REFERENCES system.services(service_id),
    target_id UUID NOT NULL REFERENCES system.service_monitor_targets(target_id),
    observed_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    check_status VARCHAR(20) NOT NULL,
    raw_result JSONB NOT NULL,
    diagnostic_fields JSONB NOT NULL
);

CREATE INDEX service_monitor_points_service_seq ON system.service_monitor_points (service_id, point_seq);
CREATE INDEX service_monitor_points_target_time ON system.service_monitor_points (target_id, observed_at DESC);

CREATE TABLE system.episodes (
    episode_id UUID PRIMARY KEY,
    service_id UUID NOT NULL REFERENCES system.services(service_id),
    status VARCHAR(20) NOT NULL,
    resolution_pending BOOLEAN NOT NULL DEFAULT FALSE,
    started_by VARCHAR(20) NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    archived_at TIMESTAMPTZ,
    monitor_start_point_seq BIGINT,
    monitor_point_watermark BIGINT,
    current_monitor_projection JSONB NOT NULL DEFAULT '[]',
    input_hash CHAR(64) NOT NULL DEFAULT '',
    input_version BIGINT NOT NULL DEFAULT 0,
    processed_input_version BIGINT NOT NULL DEFAULT 0,
    final_input_version BIGINT,
    resolution_pending_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT episode_status_chk CHECK (status IN ('ACTIVE', 'RESOLVED', 'ARCHIVED')),
    CONSTRAINT episode_started_by_chk CHECK (started_by IN ('MONITOR', 'APP'))
);

CREATE UNIQUE INDEX uniq_active_episode_per_service
    ON system.episodes (service_id)
    WHERE status = 'ACTIVE';

CREATE TABLE system.application_event_receipts (
    receipt_id UUID PRIMARY KEY,
    service_id UUID NOT NULL REFERENCES system.services(service_id),
    producer_id VARCHAR(100) NOT NULL,
    event_id VARCHAR(128) NOT NULL,
    request_hash CHAR(64) NOT NULL,
    episode_id UUID,
    response_status INTEGER NOT NULL,
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (service_id, producer_id, event_id)
);

CREATE TABLE system.episode_app_events (
    event_seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_record_id UUID UNIQUE NOT NULL,
    episode_id UUID NOT NULL REFERENCES system.episodes(episode_id),
    service_id UUID NOT NULL REFERENCES system.services(service_id),
    producer_id VARCHAR(100) NOT NULL,
    event_id VARCHAR(128) NOT NULL,
    event_kind VARCHAR(30) NOT NULL,
    health_status VARCHAR(20),
    event_time TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    source_revision BIGINT,
    schema_version INTEGER NOT NULL,
    payload JSONB NOT NULL,
    CONSTRAINT app_event_kind_chk CHECK (event_kind IN ('HEALTH_STATUS', 'DIAGNOSTIC_INFO')),
    UNIQUE (service_id, producer_id, event_id)
);

CREATE TABLE system.agent_tasks (
    task_id UUID PRIMARY KEY,
    episode_id UUID NOT NULL REFERENCES system.episodes(episode_id),
    task_type VARCHAR(20) NOT NULL,
    status VARCHAR(20) NOT NULL,
    input_version_from BIGINT,
    input_version_to BIGINT,
    final_input_version BIGINT,
    execution_generation BIGINT NOT NULL DEFAULT 1,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL,
    lease_until TIMESTAMPTZ,
    next_retry_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    last_error_code VARCHAR(100),
    last_error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT task_type_chk CHECK (task_type IN ('DIAGNOSIS', 'ARCHIVE')),
    CONSTRAINT task_status_chk CHECK (status IN ('PENDING', 'DISPATCHED', 'RUNNING', 'RETRY_WAIT', 'SUCCEEDED', 'FAILED'))
);

CREATE UNIQUE INDEX uniq_active_agent_task_per_episode
    ON system.agent_tasks (episode_id)
    WHERE status IN ('PENDING', 'DISPATCHED', 'RUNNING', 'RETRY_WAIT');

CREATE TABLE system.episode_input_versions (
    episode_id UUID NOT NULL REFERENCES system.episodes(episode_id),
    input_version BIGINT NOT NULL,
    input_hash CHAR(64) NOT NULL,
    monitor_projection JSONB NOT NULL,
    monitor_point_watermark BIGINT,
    app_event_watermark BIGINT,
    trigger_sources TEXT[] NOT NULL,
    changed_refs JSONB NOT NULL,
    assigned_task_id UUID REFERENCES system.agent_tasks(task_id),
    queue_status VARCHAR(20) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ,
    PRIMARY KEY (episode_id, input_version),
    CONSTRAINT input_queue_status_chk CHECK (queue_status IN ('PENDING', 'ASSIGNED', 'PROCESSED'))
);

CREATE TABLE system.episode_artifact_refs (
    episode_id UUID NOT NULL REFERENCES system.episodes(episode_id),
    artifact_type VARCHAR(30) NOT NULL,
    artifact_version BIGINT NOT NULL,
    storage_key VARCHAR(500) NOT NULL,
    content_type VARCHAR(30) NOT NULL,
    content_hash CHAR(64) NOT NULL,
    updated_by_task_id UUID,
    execution_generation BIGINT,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (episode_id, artifact_type),
    CONSTRAINT artifact_type_chk CHECK (artifact_type IN ('INVESTIGATION', 'DIAGNOSIS_REPORT'))
);

CREATE TABLE system.outbox_events (
    outbox_id UUID PRIMARY KEY,
    event_id UUID UNIQUE NOT NULL,
    aggregate_type VARCHAR(30) NOT NULL,
    aggregate_id UUID NOT NULL,
    event_type VARCHAR(100) NOT NULL,
    schema_version INTEGER NOT NULL,
    payload JSONB NOT NULL,
    publish_attempts INTEGER NOT NULL DEFAULT 0,
    next_retry_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ
);

CREATE INDEX outbox_unpublished ON system.outbox_events (next_retry_at, created_at)
    WHERE published_at IS NULL;

CREATE TABLE cognitive.agent_task_receipts (
    task_id UUID PRIMARY KEY,
    episode_id UUID NOT NULL,
    task_type VARCHAR(20) NOT NULL,
    execution_generation BIGINT NOT NULL,
    thread_id VARCHAR(200) NOT NULL,
    status VARCHAR(20) NOT NULL,
    input_version_from BIGINT,
    input_version_to BIGINT,
    final_input_version BIGINT,
    accepted_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    result_json JSONB,
    callback_state VARCHAR(20) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT receipt_status_chk CHECK (status IN ('ACCEPTED', 'RUNNING', 'COMPLETED', 'FAILED')),
    CONSTRAINT receipt_callback_chk CHECK (callback_state IN ('NOT_SENT', 'SENT', 'CONFIRMED'))
);

CREATE TABLE cognitive.episode_artifacts (
    episode_id UUID NOT NULL,
    artifact_type VARCHAR(30) NOT NULL,
    version BIGINT NOT NULL,
    content_type VARCHAR(30) NOT NULL,
    content_text TEXT,
    content_json JSONB,
    content_hash CHAR(64) NOT NULL,
    updated_by_task_id UUID,
    execution_generation BIGINT,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (episode_id, artifact_type),
    CONSTRAINT cog_artifact_type_chk CHECK (artifact_type IN ('INVESTIGATION', 'DIAGNOSIS_REPORT'))
);

CREATE TABLE cognitive.fault_documents (
    fd_id UUID PRIMARY KEY,
    episode_id UUID NOT NULL UNIQUE,
    service_id UUID NOT NULL,
    archived_at TIMESTAMPTZ NOT NULL,
    investigation_content TEXT NOT NULL,
    diagnosis_report JSONB NOT NULL,
    search_index JSONB NOT NULL,
    search_text TEXT NOT NULL,
    search_vector TSVECTOR,
    embedding vector(384),
    embedding_model VARCHAR(100) NOT NULL,
    index_status VARCHAR(20) NOT NULL,
    index_error TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT fd_index_status_chk CHECK (index_status IN ('BUILDING', 'READY', 'FAILED'))
);

CREATE INDEX fault_documents_service_time ON cognitive.fault_documents (service_id, archived_at DESC);
CREATE INDEX fault_documents_search ON cognitive.fault_documents USING GIN (search_vector);

-- +goose Down
DROP TABLE IF EXISTS cognitive.fault_documents;
DROP TABLE IF EXISTS cognitive.episode_artifacts;
DROP TABLE IF EXISTS cognitive.agent_task_receipts;
DROP TABLE IF EXISTS system.outbox_events;
DROP TABLE IF EXISTS system.episode_artifact_refs;
DROP TABLE IF EXISTS system.episode_input_versions;
DROP TABLE IF EXISTS system.agent_tasks;
DROP TABLE IF EXISTS system.episode_app_events;
DROP TABLE IF EXISTS system.application_event_receipts;
DROP TABLE IF EXISTS system.episodes;
DROP TABLE IF EXISTS system.service_monitor_points;
DROP TABLE IF EXISTS system.service_source_states;
DROP TABLE IF EXISTS system.service_monitor_target_states;
DROP TABLE IF EXISTS system.service_monitor_targets;
DROP TABLE IF EXISTS system.services;
DROP SCHEMA IF EXISTS cognitive;
DROP SCHEMA IF EXISTS system;
