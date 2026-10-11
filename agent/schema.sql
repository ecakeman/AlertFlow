CREATE TABLE IF NOT EXISTS runs (
    id uuid PRIMARY KEY,
    dedupe_key text UNIQUE NOT NULL,
    incident_id bigint,
    service_id text NOT NULL,
    event_type text NOT NULL,
    mode text NOT NULL,
    status text NOT NULL,
    stage text NOT NULL DEFAULT '',
    stop_reason text,
    failure_reason text,
    input_event jsonb NOT NULL,
    usage jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz
);

CREATE TABLE IF NOT EXISTS run_events (
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq int NOT NULL,
    type text NOT NULL,
    payload jsonb NOT NULL,
    ts timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS run_evidence (
    run_id uuid PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    evidence jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS reports (
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    version int NOT NULL,
    report jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, version)
);
