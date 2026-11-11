-- +goose Up
-- KUBERNETES 目标类型：扩展现有 HTTP/TCP 检查约束。
ALTER TABLE system.service_monitor_targets DROP CONSTRAINT IF EXISTS targets_type_chk;
ALTER TABLE system.service_monitor_targets
    ADD CONSTRAINT targets_type_chk CHECK (target_type IN ('HTTP','TCP','KUBERNETES'));

-- 软删除：服务与监控目标各自记录删除时间，历史 Episode/点位保留。
ALTER TABLE system.services ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE system.service_monitor_targets ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- 全局唯一与目标唯一改为仅对未删除行生效，允许同名重建。
ALTER TABLE system.services DROP CONSTRAINT IF EXISTS services_service_key_key;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_services_service_key_active
    ON system.services (service_key) WHERE deleted_at IS NULL;

ALTER TABLE system.service_monitor_targets
    DROP CONSTRAINT IF EXISTS service_monitor_targets_service_id_target_key_key;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_targets_service_key_active
    ON system.service_monitor_targets (service_id, target_key) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS system.uniq_targets_service_key_active;
DROP INDEX IF EXISTS system.uniq_services_service_key_active;

-- 重建原先的唯一约束（要求不存在软删除造成的同名重复行）。
ALTER TABLE system.service_monitor_targets
    ADD CONSTRAINT service_monitor_targets_service_id_target_key_key UNIQUE (service_id, target_key);
ALTER TABLE system.services
    ADD CONSTRAINT services_service_key_key UNIQUE (service_key);

ALTER TABLE system.service_monitor_targets DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE system.services DROP COLUMN IF EXISTS deleted_at;

ALTER TABLE system.service_monitor_targets DROP CONSTRAINT IF EXISTS targets_type_chk;
ALTER TABLE system.service_monitor_targets
    ADD CONSTRAINT targets_type_chk CHECK (target_type IN ('HTTP','TCP'));
