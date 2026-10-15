-- Agent 运行库：一次诊断的运行为主体（runs / run_events / run_evidence / reports）
-- 加长期记忆（episodes / service_profile / episode_corrections）。
-- 全部 IF NOT EXISTS，可重复执行。向量扩展由 pgvector 镜像提供。

CREATE EXTENSION IF NOT EXISTS vector;

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

CREATE UNIQUE INDEX IF NOT EXISTS run_resolve_event_id
    ON run_events (run_id, (payload->>'event_id'))
    WHERE type = 'incident.resolved' AND payload ? 'event_id';

CREATE TABLE IF NOT EXISTS reports (
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    version int NOT NULL,
    report jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, version)
);

-- ============================================================================
-- 回传 AlertFlow 的待发消息
-- ============================================================================

-- 报告写入与回调入队在**同一个事务**里完成：不会出现「报告已经落库、进程在发出
-- 回调前挂掉、之后再也不回传」的窗口。发送成功才标 sent；失败按退避重试，超过
-- 次数上限标 dead。发送是 at-least-once，AlertFlow 按 message_id 幂等。
-- 单进程 Agent，所以用 lease_until 而不是 lease_owner 防重复领取。
CREATE TABLE IF NOT EXISTS agent_callback_outbox (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id      text UNIQUE NOT NULL,
    message_type    text NOT NULL,
    correlation_id  text NOT NULL DEFAULT '',
    payload         jsonb NOT NULL,
    status          text NOT NULL DEFAULT 'pending',
    attempt_count   int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,
    lease_until     timestamptz,
    sent_at         timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_callback_due
    ON agent_callback_outbox (next_attempt_at)
    WHERE status IN ('pending', 'retry_wait', 'sending');

-- ============================================================================
-- 排查问题（异步聊天）
-- ============================================================================

-- 问题先落库、再由 worker 回答、答案经回调 outbox 回传。持久化的意义在于
-- Agent 重启后未回答的问题会被接着处理，而不是凭空消失。
CREATE TABLE IF NOT EXISTS chat_requests (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id      text UNIQUE NOT NULL,
    incident_id     bigint NOT NULL,
    message         text NOT NULL,
    history         jsonb NOT NULL DEFAULT '[]'::jsonb,
    status          text NOT NULL DEFAULT 'pending',
    attempt_count   int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_until     timestamptz,
    reply           text,
    evidence_ids    jsonb,
    error           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS chat_requests_due
    ON chat_requests (next_attempt_at)
    WHERE status IN ('pending', 'retry_wait', 'sending');

-- ============================================================================
-- 长期记忆
-- ============================================================================

-- 事件簿：每次诊断落一条。symptom/signature/conclusion 是结构化的（写入前由
-- 模型校验）；card 是规范化文本，专供嵌入与人工阅读；embedding 供向量召回。
-- 去重键用 (service_id, target_endpoint, profile_key, incident_id) 三元组 + incident。
CREATE TABLE IF NOT EXISTS episodes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id text NOT NULL,
    target_endpoint text NOT NULL DEFAULT '',
    profile_key text NOT NULL DEFAULT '',
    incident_id bigint,
    run_id uuid REFERENCES runs(id) ON DELETE SET NULL,
    started_at timestamptz,
    resolved_at timestamptz,
    duration_s int,
    severity text,
    outcome text NOT NULL DEFAULT 'unknown',
    symptom jsonb NOT NULL DEFAULT '{}'::jsonb,
    signature jsonb NOT NULL DEFAULT '{}'::jsonb,
    conclusion jsonb NOT NULL DEFAULT '{}'::jsonb,
    evidence_ids text[] NOT NULL DEFAULT '{}',
    card text NOT NULL DEFAULT '',
    card_hash text NOT NULL DEFAULT '',
    embedding vector(1024),
    embed_model text NOT NULL DEFAULT '',
    version int NOT NULL DEFAULT 1,
    superseded_by uuid REFERENCES episodes(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (service_id, target_endpoint, profile_key, incident_id)
);

-- 主检索轴是服务；hnsw 支持余弦近邻。
CREATE INDEX IF NOT EXISTS episodes_service_started ON episodes (service_id, started_at DESC);
CREATE INDEX IF NOT EXISTS episodes_embedding ON episodes USING hnsw (embedding vector_cosine_ops);

-- 服务档案：每服务一份，从事件簿派生。baseline 由原始数据统计，known_* 由事件聚合。
CREATE TABLE IF NOT EXISTS service_profile (
    service_id text PRIMARY KEY,
    baseline jsonb NOT NULL DEFAULT '{}'::jsonb,
    known_causes jsonb NOT NULL DEFAULT '[]'::jsonb,
    known_patterns jsonb NOT NULL DEFAULT '[]'::jsonb,
    episode_count int NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 修正链：记忆会错，错的记忆必须能被改。每次改判留一条。
CREATE TABLE IF NOT EXISTS episode_corrections (
    id bigserial PRIMARY KEY,
    episode_id uuid NOT NULL REFERENCES episodes(id) ON DELETE CASCADE,
    field text NOT NULL,
    from_value jsonb,
    to_value jsonb,
    reason text,
    created_at timestamptz NOT NULL DEFAULT now()
);
