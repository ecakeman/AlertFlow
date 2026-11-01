package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const (
	StatusUnknown  = "UNKNOWN"
	StatusNormal   = "NORMAL"
	StatusAbnormal = "ABNORMAL"

	SourceMonitor = "MONITOR"
	SourceApp     = "APP"

	EpisodeActive   = "ACTIVE"
	EpisodeResolved = "RESOLVED"
	EpisodeArchived = "ARCHIVED"

	TaskPending    = "PENDING"
	TaskDispatched = "DISPATCHED"
	TaskRunning    = "RUNNING"
	TaskRetryWait  = "RETRY_WAIT"
	TaskSucceeded  = "SUCCEEDED"
	TaskFailed     = "FAILED"

	TaskDiagnosis = "DIAGNOSIS"
	TaskArchive   = "ARCHIVE"
)

// Store 是系统侧事务入口。同一 Service 的写先锁 Service 行。
type Store struct {
	DB          *sql.DB
	Now         func() time.Time
	MaxAttempts int
	Lease       time.Duration
	RetryBase   time.Duration
}

func New(db *sql.DB) *Store {
	return &Store{
		DB:          db,
		Now:         func() time.Time { return time.Now().UTC() },
		MaxAttempts: 3,
		Lease:       30 * time.Second,
		RetryBase:   time.Second,
	}
}

func (s *Store) now() time.Time { return s.Now().UTC() }

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// TargetInput 是创建服务时的监控目标。
type TargetInput struct {
	TargetKey         string          `json:"target_key"`
	TargetName        string          `json:"target_name"`
	TargetType        string          `json:"target_type"`
	Address           string          `json:"address"`
	Config            json.RawMessage `json:"config"`
	IntervalMS        int             `json:"interval_ms"`
	TimeoutMS         int             `json:"timeout_ms"`
	FailureThreshold  int             `json:"failure_threshold"`
	RecoveryThreshold int             `json:"recovery_threshold"`
	Required          *bool           `json:"is_required"`
	Enabled           *bool           `json:"enabled"`
}

// CreateServiceInput 创建 Service，并初始化 MONITOR 与 APP 两条来源。
type CreateServiceInput struct {
	ServiceKey          string        `json:"service_key"`
	Name                string        `json:"name"`
	Description         string        `json:"description"`
	AuthorityProducerID string        `json:"authority_producer_id"`
	Targets             []TargetInput `json:"targets"`
}

// Target 是已保存的监控目标。
type Target struct {
	TargetID             uuid.UUID       `json:"target_id"`
	TargetKey            string          `json:"target_key"`
	TargetName           string          `json:"target_name"`
	TargetType           string          `json:"target_type"`
	Address              string          `json:"address"`
	Config               json.RawMessage `json:"config"`
	IntervalMS           int             `json:"interval_ms"`
	TimeoutMS            int             `json:"timeout_ms"`
	FailureThreshold     int             `json:"failure_threshold"`
	RecoveryThreshold    int             `json:"recovery_threshold"`
	Required             bool            `json:"is_required"`
	Enabled              bool            `json:"enabled"`
	Status               string          `json:"status"`
	ConsecutiveFailures  int             `json:"consecutive_failures"`
	ConsecutiveSuccesses int             `json:"consecutive_successes"`
}

// ServiceView 是管理查询返回的服务与双来源状态。
type ServiceView struct {
	ServiceID           uuid.UUID `json:"service_id"`
	ServiceKey          string    `json:"service_key"`
	Name                string    `json:"name"`
	Description         string    `json:"description"`
	Enabled             bool      `json:"enabled"`
	AggregationMode     string    `json:"monitor_aggregation_mode"`
	AuthorityProducerID string    `json:"authority_producer_id"`
	MonitorStatus       string    `json:"monitor_status"`
	AppStatus           string    `json:"app_status"`
	Targets             []Target  `json:"targets"`
}

func (s *Store) CreateService(ctx context.Context, in CreateServiceInput) (ServiceView, error) {
	if in.ServiceKey == "" || in.Name == "" || in.AuthorityProducerID == "" {
		return ServiceView{}, errCode("VALIDATION", "service_key, name and authority_producer_id are required", 400)
	}
	if len(in.Targets) == 0 {
		return ServiceView{}, errCode("VALIDATION", "at least one monitor target is required", 400)
	}
	id := uuid.New()
	now := s.now()
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO system.services
			(service_id, service_key, name, description, enabled, monitor_aggregation_mode, authority_producer_id, created_at, updated_at)
			VALUES ($1,$2,$3,$4,TRUE,'ANY_REQUIRED',$5,$6,$6)`,
			id, in.ServiceKey, in.Name, in.Description, in.AuthorityProducerID, now)
		if err != nil {
			return err
		}
		for _, src := range []string{SourceMonitor, SourceApp} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO system.service_source_states (service_id, source, status, updated_at)
				VALUES ($1,$2,'UNKNOWN',$3)`, id, src, now); err != nil {
				return err
			}
		}
		for _, t := range in.Targets {
			if err := insertTarget(ctx, tx, id, t, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ServiceView{}, err
	}
	return s.GetService(ctx, id)
}

func insertTarget(ctx context.Context, tx *sql.Tx, serviceID uuid.UUID, t TargetInput, now time.Time) error {
	if t.TargetKey == "" || t.Address == "" || (t.TargetType != "HTTP" && t.TargetType != "TCP") {
		return errCode("VALIDATION", "target_key, address and target_type HTTP|TCP are required", 400)
	}
	if t.FailureThreshold <= 0 || t.RecoveryThreshold <= 0 || t.IntervalMS <= 0 || t.TimeoutMS <= 0 {
		return errCode("VALIDATION", "interval, timeout and thresholds must be positive", 400)
	}
	required, enabled := true, true
	if t.Required != nil {
		required = *t.Required
	}
	if t.Enabled != nil {
		enabled = *t.Enabled
	}
	cfg := t.Config
	if len(cfg) == 0 {
		cfg = []byte(`{}`)
	}
	tid := uuid.New()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO system.service_monitor_targets
		(target_id, service_id, target_key, target_name, target_type, address, config, interval_ms, timeout_ms,
		 failure_threshold, recovery_threshold, is_required, enabled, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)`,
		tid, serviceID, t.TargetKey, t.TargetName, t.TargetType, t.Address, cfg, t.IntervalMS, t.TimeoutMS,
		t.FailureThreshold, t.RecoveryThreshold, required, enabled, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO system.service_monitor_target_states
		(target_id, status, consecutive_failures, consecutive_successes, updated_at)
		VALUES ($1,'UNKNOWN',0,0,$2)`, tid, now)
	return err
}

func (s *Store) GetService(ctx context.Context, id uuid.UUID) (ServiceView, error) {
	var v ServiceView
	err := s.DB.QueryRowContext(ctx, `
		SELECT s.service_id, s.service_key, s.name, COALESCE(s.description,''), s.enabled,
		       s.monitor_aggregation_mode, s.authority_producer_id,
		       m.status, a.status
		FROM system.services s
		JOIN system.service_source_states m ON m.service_id=s.service_id AND m.source='MONITOR'
		JOIN system.service_source_states a ON a.service_id=s.service_id AND a.source='APP'
		WHERE s.service_id=$1`, id).Scan(
		&v.ServiceID, &v.ServiceKey, &v.Name, &v.Description, &v.Enabled, &v.AggregationMode,
		&v.AuthorityProducerID, &v.MonitorStatus, &v.AppStatus)
	if err == sql.ErrNoRows {
		return ServiceView{}, errCode("SERVICE_NOT_FOUND", "service not found", 404)
	}
	if err != nil {
		return ServiceView{}, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.target_id, t.target_key, t.target_name, t.target_type, t.address, t.config,
		       t.interval_ms, t.timeout_ms, t.failure_threshold, t.recovery_threshold,
		       t.is_required, t.enabled, st.status, st.consecutive_failures, st.consecutive_successes
		FROM system.service_monitor_targets t
		JOIN system.service_monitor_target_states st ON st.target_id=t.target_id
		WHERE t.service_id=$1
		ORDER BY t.target_key`, id)
	if err != nil {
		return ServiceView{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.TargetID, &t.TargetKey, &t.TargetName, &t.TargetType, &t.Address, &t.Config,
			&t.IntervalMS, &t.TimeoutMS, &t.FailureThreshold, &t.RecoveryThreshold,
			&t.Required, &t.Enabled, &t.Status, &t.ConsecutiveFailures, &t.ConsecutiveSuccesses); err != nil {
			return ServiceView{}, err
		}
		v.Targets = append(v.Targets, t)
	}
	return v, rows.Err()
}

// PatchServiceInput 只改服务元数据，不改已经写入的时间轴。
type PatchServiceInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Enabled     *bool   `json:"enabled"`
}

func (s *Store) PatchService(ctx context.Context, id uuid.UUID, in PatchServiceInput) (ServiceView, error) {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockService(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE system.services
			SET name = COALESCE($2::varchar, name),
			    description = COALESCE($3::text, description),
			    enabled = COALESCE($4::boolean, enabled),
			    updated_at = $5
			WHERE service_id=$1`, id, in.Name, in.Description, in.Enabled, s.now())
		return err
	})
	if err != nil {
		return ServiceView{}, err
	}
	return s.GetService(ctx, id)
}

func lockService(ctx context.Context, tx *sql.Tx, id uuid.UUID) error {
	var got uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT service_id FROM system.services WHERE service_id=$1 FOR UPDATE`, id).Scan(&got)
	if err == sql.ErrNoRows {
		return errCode("SERVICE_NOT_FOUND", "service not found", 404)
	}
	return err
}
