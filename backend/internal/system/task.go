package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// TaskAck 是 gRPC 回报的处理结果。accepted 为 false 时事务不修改状态。
type TaskAck struct {
	Accepted bool
	TaskID   uuid.UUID
	Message  string
}

type taskRow struct {
	ID           uuid.UUID
	EpisodeID    uuid.UUID
	Type         string
	Status       string
	From         sql.NullInt64
	To           sql.NullInt64
	Final        sql.NullInt64
	Generation   int64
	AttemptCount int
	MaxAttempts  int
}

func (s *Store) ensureDiagnosisTask(ctx context.Context, tx *sql.Tx, ep *episodeRow, now time.Time) error {
	active, err := countActiveTasks(ctx, tx, ep.ID)
	if err != nil || active > 0 {
		return err
	}
	var maxV sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(input_version) FROM system.episode_input_versions
		WHERE episode_id=$1 AND queue_status='PENDING'`, ep.ID).Scan(&maxV); err != nil {
		return err
	}
	if !maxV.Valid || maxV.Int64 <= ep.ProcessedInputVersion {
		return nil
	}
	from := ep.ProcessedInputVersion + 1
	to := maxV.Int64
	var cnt int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM system.episode_input_versions
		WHERE episode_id=$1 AND input_version BETWEEN $2 AND $3 AND queue_status='PENDING'`,
		ep.ID, from, to).Scan(&cnt); err != nil {
		return err
	}
	if int64(cnt) != to-from+1 {
		return errCode("INPUT_GAP", "unprocessed input versions are not contiguous", 500)
	}
	return s.insertTask(ctx, tx, ep, TaskDiagnosis, from, to, sql.NullInt64{}, 1, now)
}

func (s *Store) tryResolve(ctx context.Context, tx *sql.Tx, ep *episodeRow, monitor, app string, now time.Time) error {
	if ep.Status != EpisodeActive || !ep.ResolutionPending {
		return nil
	}
	if monitor != StatusNormal || app != StatusNormal {
		return nil
	}
	if !ep.FinalInputVersion.Valid || ep.ProcessedInputVersion != ep.FinalInputVersion.Int64 {
		return nil
	}
	active, err := countActiveTasks(ctx, tx, ep.ID)
	if err != nil || active > 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE system.episodes
		SET status='RESOLVED', resolution_pending=FALSE, resolved_at=$2, updated_at=$2
		WHERE episode_id=$1 AND status='ACTIVE'`, ep.ID, now); err != nil {
		return err
	}
	ep.Status = EpisodeResolved
	ep.ResolutionPending = false
	return s.insertTask(ctx, tx, ep, TaskArchive, 0, 0, ep.FinalInputVersion, 1, now)
}

func (s *Store) insertTask(ctx context.Context, tx *sql.Tx, ep *episodeRow, taskType string, from, to int64, final sql.NullInt64, generation int64, now time.Time) error {
	taskID := uuid.New()
	var fromV, toV sql.NullInt64
	if taskType == TaskDiagnosis {
		fromV = sql.NullInt64{Int64: from, Valid: true}
		toV = sql.NullInt64{Int64: to, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO system.agent_tasks
		(task_id, episode_id, task_type, status, input_version_from, input_version_to, final_input_version,
		 execution_generation, attempt_count, max_attempts, created_at, updated_at)
		VALUES ($1,$2,$3,'PENDING',$4,$5,$6,$7,0,$8,$9,$9)`,
		taskID, ep.ID, taskType, fromV, toV, final, generation, s.MaxAttempts, now)
	if err != nil {
		return err
	}
	if taskType == TaskDiagnosis {
		if _, err := tx.ExecContext(ctx, `
			UPDATE system.episode_input_versions
			SET assigned_task_id=$2, queue_status='ASSIGNED'
			WHERE episode_id=$1 AND input_version BETWEEN $3 AND $4 AND queue_status='PENDING'`,
			ep.ID, taskID, from, to); err != nil {
			return err
		}
	}
	return insertOutbox(ctx, tx, taskID, ep.ID, taskType, fromV, toV, generation, now)
}

func insertOutbox(ctx context.Context, tx *sql.Tx, taskID, episodeID uuid.UUID, taskType string, from, to sql.NullInt64, generation int64, now time.Time) error {
	eventID := uuid.New()
	payload := map[string]any{
		"event_id":             eventID.String(),
		"event_type":           "AGENT_TASK_READY",
		"schema_version":       1,
		"task_id":              taskID.String(),
		"episode_id":           episodeID.String(),
		"task_type":            taskType,
		"input_version_from":   nullInt(from),
		"input_version_to":     nullInt(to),
		"execution_generation": generation,
		"created_at":           now.Format(time.RFC3339Nano),
	}
	raw, _ := json.Marshal(payload)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO system.outbox_events
		(outbox_id, event_id, aggregate_type, aggregate_id, event_type, schema_version, payload, next_retry_at, created_at)
		VALUES ($1,$2,'AGENT_TASK',$3,'AGENT_TASK_READY',1,$4,$5,$5)`,
		uuid.New(), eventID, taskID, raw, now)
	return err
}

func nullInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func countActiveTasks(ctx context.Context, tx *sql.Tx, episodeID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM system.agent_tasks
		WHERE episode_id=$1 AND status IN ('PENDING','DISPATCHED','RUNNING','RETRY_WAIT')`, episodeID).Scan(&n)
	return n, err
}

func (s *Store) ReportTaskStarted(ctx context.Context, taskID, episodeID uuid.UUID, generation int64) (TaskAck, error) {
	return s.touchTask(ctx, taskID, episodeID, generation, true)
}

func (s *Store) HeartbeatTask(ctx context.Context, taskID, episodeID uuid.UUID, generation int64) (TaskAck, error) {
	return s.touchTask(ctx, taskID, episodeID, generation, false)
}

func (s *Store) touchTask(ctx context.Context, taskID, episodeID uuid.UUID, generation int64, start bool) (TaskAck, error) {
	ack := TaskAck{TaskID: taskID}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ep, task, err := s.lockTask(ctx, tx, taskID, episodeID)
		if err != nil {
			return err
		}
		if task.Generation != generation {
			ack.Message = "stale execution generation"
			return nil
		}
		if task.Status == TaskSucceeded || task.Status == TaskFailed {
			ack.Message = "task already finished"
			return nil
		}
		now := s.now()
		lease := now.Add(s.Lease)
		if start {
			if task.Status != TaskPending && task.Status != TaskDispatched && task.Status != TaskRunning {
				ack.Message = "task is not startable"
				return nil
			}
			_, err = tx.ExecContext(ctx, `
				UPDATE system.agent_tasks
				SET status='RUNNING', started_at=COALESCE(started_at,$2), lease_until=$3, updated_at=$2
				WHERE task_id=$1`, taskID, now, lease)
			ack.Message = "started"
		} else if task.Status != TaskRunning {
			ack.Message = "task is not running"
			return nil
		} else {
			_, err = tx.ExecContext(ctx, `
				UPDATE system.agent_tasks SET lease_until=$2, updated_at=$3 WHERE task_id=$1`, taskID, lease, now)
			ack.Message = "heartbeat"
		}
		if err != nil {
			return err
		}
		_ = ep
		ack.Accepted = true
		return nil
	})
	return ack, err
}

// CompleteInput 是 Python 的任务完成回报。
type CompleteInput struct {
	TaskID                uuid.UUID
	EpisodeID             uuid.UUID
	TaskType              string
	Generation            int64
	Outcome               string
	ProcessedInputVersion int64
	FDID                  string
	ErrorCode             string
	ErrorMessage          string
}

func (s *Store) CompleteTask(ctx context.Context, in CompleteInput) (TaskAck, error) {
	ack := TaskAck{TaskID: in.TaskID}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ep, task, err := s.lockTask(ctx, tx, in.TaskID, in.EpisodeID)
		if err != nil {
			return err
		}
		if task.Generation != in.Generation {
			ack.Message = "stale execution generation"
			return nil
		}
		if task.Type != in.TaskType {
			ack.Message = "task type mismatch"
			return nil
		}
		if task.Status == TaskSucceeded && in.Outcome == "SUCCEEDED" {
			ack.Accepted = true
			ack.Message = "duplicate completion"
			return nil
		}
		if task.Status == TaskFailed || task.Status == TaskSucceeded {
			ack.Message = "task already finished"
			return nil
		}
		now := s.now()
		switch in.Outcome {
		case "SUCCEEDED":
			if task.Type == TaskDiagnosis {
				if !task.To.Valid || in.ProcessedInputVersion != task.To.Int64 {
					ack.Message = "processed_input_version must equal input_version_to"
					return nil
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE system.episode_input_versions
					SET queue_status='PROCESSED', processed_at=$2
					WHERE episode_id=$1 AND input_version BETWEEN $3 AND $4`,
					ep.ID, now, task.From.Int64, task.To.Int64); err != nil {
					return err
				}
				ep.ProcessedInputVersion = task.To.Int64
				if _, err := tx.ExecContext(ctx, `
					UPDATE system.episodes SET processed_input_version=$2, updated_at=$3 WHERE episode_id=$1`,
					ep.ID, ep.ProcessedInputVersion, now); err != nil {
					return err
				}
			} else {
				if in.FDID == "" {
					ack.Message = "archive success requires fd_id"
					return nil
				}
				ready, err := archiveReady(ctx, tx, ep.ID, in.TaskID, in.Generation, in.FDID)
				if err != nil {
					return err
				}
				if !ready {
					ack.Message = "fault document is not READY"
					return nil
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE system.agent_tasks
				SET status='SUCCEEDED', finished_at=$2, lease_until=NULL, updated_at=$2
				WHERE task_id=$1`, in.TaskID, now); err != nil {
				return err
			}
			if task.Type == TaskArchive {
				if _, err := tx.ExecContext(ctx, `
					UPDATE system.episodes
					SET status='ARCHIVED', archived_at=$2, updated_at=$2
					WHERE episode_id=$1 AND status='RESOLVED'`, ep.ID, now); err != nil {
					return err
				}
				ep.Status = EpisodeArchived
			} else {
				monitor, err := sourceStatus(ctx, tx, ep.ServiceID, SourceMonitor)
				if err != nil {
					return err
				}
				app, err := sourceStatus(ctx, tx, ep.ServiceID, SourceApp)
				if err != nil {
					return err
				}
				if err := s.tryResolve(ctx, tx, ep, monitor, app, now); err != nil {
					return err
				}
				if ep.Status == EpisodeActive {
					if err := s.ensureDiagnosisTask(ctx, tx, ep, now); err != nil {
						return err
					}
				}
			}
			ack.Accepted = true
			ack.Message = "completed"
		case "RETRYABLE_FAILURE", "PERMANENT_FAILURE":
			task.AttemptCount++
			status := TaskRetryWait
			var next sql.NullTime
			if in.Outcome == "PERMANENT_FAILURE" || task.AttemptCount >= task.MaxAttempts {
				status = TaskFailed
			} else {
				next = sql.NullTime{Time: now.Add(time.Duration(task.AttemptCount) * s.RetryBase), Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE system.agent_tasks
				SET status=$2::varchar, attempt_count=$3, next_retry_at=$4::timestamptz, lease_until=NULL,
				    last_error_code=$5::varchar, last_error_message=$6::text,
				    finished_at=CASE WHEN $2::varchar='FAILED' THEN $7::timestamptz ELSE NULL::timestamptz END,
				    updated_at=$7
				WHERE task_id=$1`,
				in.TaskID, status, task.AttemptCount, next, nullStr(in.ErrorCode), nullStr(in.ErrorMessage), now); err != nil {
				return err
			}
			if status == TaskRetryWait {
				if _, err := tx.ExecContext(ctx, `
					UPDATE system.episode_input_versions
					SET queue_status='PENDING', assigned_task_id=NULL
					WHERE episode_id=$1 AND assigned_task_id=$2 AND queue_status='ASSIGNED'`,
					ep.ID, in.TaskID); err != nil {
					return err
				}
			}
			ack.Accepted = true
			ack.Message = status
		default:
			ack.Message = "unknown outcome"
		}
		return nil
	})
	return ack, err
}

func archiveReady(ctx context.Context, tx *sql.Tx, episodeID, taskID uuid.UUID, generation int64, fdID string) (bool, error) {
	parsed, err := uuid.Parse(fdID)
	if err != nil {
		return false, nil
	}
	var found string
	err = tx.QueryRowContext(ctx, `
		SELECT fd_id FROM system.episode_fault_document_refs
		WHERE episode_id=$1 AND task_id=$2 AND execution_generation=$3 AND fd_id=$4`,
		episodeID, taskID, generation, parsed).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RegisterFaultDocumentInput 是 Python 在 FD 就绪后交给 Go 的回执。
type RegisterFaultDocumentInput struct {
	TaskID       uuid.UUID
	EpisodeID    uuid.UUID
	Generation   int64
	FDID         string
	ContentHash  string
}

// RegisterFaultDocument 把归档回执写入 system 表。相同内容重复登记是幂等的。
func (s *Store) RegisterFaultDocument(ctx context.Context, in RegisterFaultDocumentInput) (bool, string, error) {
	registered := false
	message := ""
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ep, task, err := s.lockTask(ctx, tx, in.TaskID, in.EpisodeID)
		if err != nil {
			return err
		}
		if ep == nil {
			return nil
		}
		if task.Type != TaskArchive {
			message = "not an archive task"
			return nil
		}
		if task.Generation != in.Generation {
			message = "stale execution generation"
			return nil
		}
		fdID, err := uuid.Parse(in.FDID)
		if err != nil || len(in.ContentHash) != 64 {
			message = "fd_id and content_hash are required"
			return nil
		}
		var existing string
		err = tx.QueryRowContext(ctx, `
			SELECT content_hash FROM system.episode_fault_document_refs
			WHERE task_id=$1 AND execution_generation=$2`, in.TaskID, in.Generation).Scan(&existing)
		if err == nil {
			if existing != in.ContentHash {
				message = "idempotency conflict"
				return nil
			}
			registered = true
			message = "duplicate"
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system.episode_fault_document_refs
			(episode_id, fd_id, task_id, execution_generation, content_hash, registered_at)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			in.EpisodeID, fdID, in.TaskID, in.Generation, in.ContentHash, s.now()); err != nil {
			return err
		}
		registered = true
		message = "registered"
		return nil
	})
	return registered, message, err
}

func (s *Store) lockTask(ctx context.Context, tx *sql.Tx, taskID, episodeID uuid.UUID) (*episodeRow, taskRow, error) {
	var serviceID uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT service_id FROM system.episodes WHERE episode_id=$1`, episodeID).Scan(&serviceID)
	if err == sql.ErrNoRows {
		return nil, taskRow{}, errCode("EPISODE_NOT_FOUND", "episode not found", 404)
	}
	if err != nil {
		return nil, taskRow{}, err
	}
	if err := lockService(ctx, tx, serviceID); err != nil {
		return nil, taskRow{}, err
	}
	ep, err := lockEpisode(ctx, tx, episodeID)
	if err != nil {
		return nil, taskRow{}, err
	}
	var task taskRow
	err = tx.QueryRowContext(ctx, `
		SELECT task_id, episode_id, task_type, status, input_version_from, input_version_to,
		       final_input_version, execution_generation, attempt_count, max_attempts
		FROM system.agent_tasks WHERE task_id=$1 FOR UPDATE`, taskID).Scan(
		&task.ID, &task.EpisodeID, &task.Type, &task.Status, &task.From, &task.To, &task.Final,
		&task.Generation, &task.AttemptCount, &task.MaxAttempts)
	if err == sql.ErrNoRows {
		return nil, taskRow{}, errCode("TASK_NOT_FOUND", "task not found", 404)
	}
	if err != nil {
		return nil, taskRow{}, err
	}
	if task.EpisodeID != episodeID {
		return nil, taskRow{}, errCode("TASK_EPISODE_MISMATCH", "task does not belong to episode", 409)
	}
	return ep, task, nil
}

// RegisterArtifactInput 只登记引用，不保存正文。
type RegisterArtifactInput struct {
	TaskID       uuid.UUID
	EpisodeID    uuid.UUID
	ArtifactType string
	Generation   int64
	Version      int64
	StorageKey   string
	ContentType  string
	ContentHash  string
}

func (s *Store) RegisterArtifact(ctx context.Context, in RegisterArtifactInput) (bool, int64, error) {
	var registered bool
	var version int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, task, err := s.lockTask(ctx, tx, in.TaskID, in.EpisodeID)
		if err != nil {
			return err
		}
		if task.Generation != in.Generation {
			return errCode("STALE_GENERATION", "stale execution generation", 409)
		}
		var cur sql.NullInt64
		var hash sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT artifact_version, content_hash FROM system.episode_artifact_refs
			WHERE episode_id=$1 AND artifact_type=$2`, in.EpisodeID, in.ArtifactType).Scan(&cur, &hash)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		now := s.now()
		if err == sql.ErrNoRows {
			if in.Version < 1 {
				return errCode("ARTIFACT_VERSION", "artifact version must start at 1", 409)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO system.episode_artifact_refs
				(episode_id, artifact_type, artifact_version, storage_key, content_type, content_hash,
				 updated_by_task_id, execution_generation, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				in.EpisodeID, in.ArtifactType, in.Version, in.StorageKey, in.ContentType, in.ContentHash,
				in.TaskID, in.Generation, now)
			registered = err == nil
			version = in.Version
			return err
		}
		if cur.Int64 == in.Version && hash.String == in.ContentHash {
			registered = true
			version = cur.Int64
			return nil
		}
		if in.Version != cur.Int64+1 {
			return errCode("ARTIFACT_VERSION", "artifact version conflict", 409)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE system.episode_artifact_refs
			SET artifact_version=$3, storage_key=$4, content_type=$5, content_hash=$6,
			    updated_by_task_id=$7, execution_generation=$8, updated_at=$9
			WHERE episode_id=$1 AND artifact_type=$2`,
			in.EpisodeID, in.ArtifactType, in.Version, in.StorageKey, in.ContentType, in.ContentHash,
			in.TaskID, in.Generation, now)
		registered = err == nil
		version = in.Version
		return err
	})
	return registered, version, err
}

// MarkDispatched 在 Python 持久化接收记录后调用。
func (s *Store) MarkDispatched(ctx context.Context, taskID uuid.UUID, generation int64) error {
	now := s.now()
	res, err := s.DB.ExecContext(ctx, `
		UPDATE system.agent_tasks
		SET status='DISPATCHED', lease_until=$3, updated_at=$4
		WHERE task_id=$1 AND execution_generation=$2 AND status='PENDING'`,
		taskID, generation, now.Add(s.Lease), now)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		return nil
	}
	var status string
	var gen int64
	err = s.DB.QueryRowContext(ctx, `SELECT status, execution_generation FROM system.agent_tasks WHERE task_id=$1`, taskID).Scan(&status, &gen)
	if err != nil {
		return err
	}
	if gen == generation && (status == TaskDispatched || status == TaskRunning || status == TaskSucceeded) {
		return nil
	}
	return errCode("DISPATCH_REJECTED", "task is not pending for this generation", 409)
}

// RecoverOnce 扫描过期租约、到期重试和缺失任务。它不实现另一套状态机。
func (s *Store) RecoverOnce(ctx context.Context) error {
	now := s.now()
	rows, err := s.DB.QueryContext(ctx, `
		SELECT task_id, episode_id FROM system.agent_tasks
		WHERE status IN ('DISPATCHED','RUNNING') AND lease_until IS NOT NULL AND lease_until < $1`, now)
	if err != nil {
		return err
	}
	type pair struct{ task, episode uuid.UUID }
	var expired []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.task, &p.episode); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, p)
	}
	rows.Close()
	for _, p := range expired {
		if err := s.expireTask(ctx, p.task, p.episode); err != nil {
			return err
		}
	}
	if err := s.promoteRetries(ctx, now); err != nil {
		return err
	}
	return s.backfillTasks(ctx, now)
}

func (s *Store) expireTask(ctx context.Context, taskID, episodeID uuid.UUID) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		ep, task, err := s.lockTask(ctx, tx, taskID, episodeID)
		if err != nil {
			return err
		}
		if task.Status != TaskDispatched && task.Status != TaskRunning {
			return nil
		}
		if task.Generation == 0 {
			return nil
		}
		now := s.now()
		if !task.From.Valid && task.Type == TaskDiagnosis {
			return nil
		}
		leaseExpired := true
		var lease sql.NullTime
		_ = tx.QueryRowContext(ctx, `SELECT lease_until FROM system.agent_tasks WHERE task_id=$1`, taskID).Scan(&lease)
		if lease.Valid && lease.Time.After(now) {
			leaseExpired = false
		}
		if !leaseExpired {
			return nil
		}
		in := CompleteInput{
			TaskID: taskID, EpisodeID: episodeID, TaskType: task.Type, Generation: task.Generation,
			Outcome: "RETRYABLE_FAILURE", ErrorCode: "LEASE_EXPIRED", ErrorMessage: "execution lease expired",
		}
		_ = ep
		return s.failLocked(ctx, tx, ep, task, in, now)
	})
}

func (s *Store) failLocked(ctx context.Context, tx *sql.Tx, ep *episodeRow, task taskRow, in CompleteInput, now time.Time) error {
	task.AttemptCount++
	status := TaskRetryWait
	var next sql.NullTime
	if task.AttemptCount >= task.MaxAttempts {
		status = TaskFailed
	} else {
		next = sql.NullTime{Time: now.Add(time.Duration(task.AttemptCount) * s.RetryBase), Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE system.agent_tasks
		SET status=$2, attempt_count=$3, next_retry_at=$4, lease_until=NULL,
		    last_error_code=$5, last_error_message=$6, updated_at=$7
		WHERE task_id=$1`,
		in.TaskID, status, task.AttemptCount, next, in.ErrorCode, in.ErrorMessage, now)
	if err != nil || status != TaskRetryWait {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE system.episode_input_versions
		SET queue_status='PENDING', assigned_task_id=NULL
		WHERE episode_id=$1 AND assigned_task_id=$2 AND queue_status='ASSIGNED'`, ep.ID, in.TaskID)
	return err
}

func (s *Store) promoteRetries(ctx context.Context, now time.Time) error {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT task_id, episode_id FROM system.agent_tasks
		WHERE status='RETRY_WAIT' AND next_retry_at IS NOT NULL AND next_retry_at <= $1`, now)
	if err != nil {
		return err
	}
	var pairs [][2]uuid.UUID
	for rows.Next() {
		var a, b uuid.UUID
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, [2]uuid.UUID{a, b})
	}
	rows.Close()
	for _, p := range pairs {
		if err := s.requeueTask(ctx, p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) requeueTask(ctx context.Context, taskID, episodeID uuid.UUID) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		ep, task, err := s.lockTask(ctx, tx, taskID, episodeID)
		if err != nil || task.Status != TaskRetryWait {
			return err
		}
		now := s.now()
		task.Generation++
		_, err = tx.ExecContext(ctx, `
			UPDATE system.agent_tasks
			SET status='PENDING', execution_generation=$2, next_retry_at=NULL, updated_at=$3
			WHERE task_id=$1`, taskID, task.Generation, now)
		if err != nil {
			return err
		}
		if task.Type == TaskDiagnosis && task.From.Valid {
			if _, err := tx.ExecContext(ctx, `
				UPDATE system.episode_input_versions
				SET assigned_task_id=$2, queue_status='ASSIGNED'
				WHERE episode_id=$1 AND input_version BETWEEN $3 AND $4 AND queue_status='PENDING'`,
				ep.ID, taskID, task.From.Int64, task.To.Int64); err != nil {
				return err
			}
		}
		return insertOutbox(ctx, tx, taskID, ep.ID, task.Type, task.From, task.To, task.Generation, now)
	})
}

func (s *Store) backfillTasks(ctx context.Context, now time.Time) error {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT e.episode_id, e.service_id
		FROM system.episodes e
		WHERE e.status='ACTIVE'
		  AND EXISTS (
		    SELECT 1 FROM system.episode_input_versions v
		    WHERE v.episode_id=e.episode_id AND v.queue_status='PENDING')
		  AND NOT EXISTS (
		    SELECT 1 FROM system.agent_tasks t
		    WHERE t.episode_id=e.episode_id AND t.status IN ('PENDING','DISPATCHED','RUNNING','RETRY_WAIT'))`)
	if err != nil {
		return err
	}
	var ids [][2]uuid.UUID
	for rows.Next() {
		var a, b uuid.UUID
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, [2]uuid.UUID{a, b})
	}
	rows.Close()
	for _, id := range ids {
		if err := s.withTx(ctx, func(tx *sql.Tx) error {
			if err := lockService(ctx, tx, id[1]); err != nil {
				return err
			}
			ep, err := lockEpisode(ctx, tx, id[0])
			if err != nil || ep.Status != EpisodeActive {
				return err
			}
			return s.ensureDiagnosisTask(ctx, tx, ep, now)
		}); err != nil {
			return err
		}
	}
	return s.backfillArchive(ctx, now)
}

func (s *Store) backfillArchive(ctx context.Context, now time.Time) error {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT episode_id, service_id FROM system.episodes e
		WHERE status='RESOLVED'
		  AND NOT EXISTS (
		    SELECT 1 FROM system.agent_tasks t
		    WHERE t.episode_id=e.episode_id AND t.task_type='ARCHIVE'
		      AND t.status IN ('PENDING','DISPATCHED','RUNNING','RETRY_WAIT','SUCCEEDED'))`)
	if err != nil {
		return err
	}
	var ids [][2]uuid.UUID
	for rows.Next() {
		var a, b uuid.UUID
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, [2]uuid.UUID{a, b})
	}
	rows.Close()
	for _, id := range ids {
		if err := s.withTx(ctx, func(tx *sql.Tx) error {
			if err := lockService(ctx, tx, id[1]); err != nil {
				return err
			}
			ep, err := lockEpisode(ctx, tx, id[0])
			if err != nil || ep.Status != EpisodeResolved {
				return err
			}
			active, err := countActiveTasks(ctx, tx, ep.ID)
			if err != nil || active > 0 {
				return err
			}
			return s.insertTask(ctx, tx, ep, TaskArchive, 0, 0, ep.FinalInputVersion, 1, now)
		}); err != nil {
			return err
		}
	}
	return nil
}

// RetryDiagnosis 为失败批次新建任务，不推进已处理版本。
func (s *Store) RetryDiagnosis(ctx context.Context, episodeID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var serviceID uuid.UUID
		if err := tx.QueryRowContext(ctx, `SELECT service_id FROM system.episodes WHERE episode_id=$1`, episodeID).Scan(&serviceID); err != nil {
			return err
		}
		if err := lockService(ctx, tx, serviceID); err != nil {
			return err
		}
		ep, err := lockEpisode(ctx, tx, episodeID)
		if err != nil {
			return err
		}
		if ep.Status != EpisodeActive {
			return errCode("EPISODE_NOT_ACTIVE", "diagnosis retry requires an active episode", 409)
		}
		active, err := countActiveTasks(ctx, tx, ep.ID)
		if err != nil {
			return err
		}
		if active > 0 {
			return errCode("TASK_ACTIVE", "an agent task is already active", 409)
		}
		var failed int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM system.agent_tasks
			WHERE episode_id=$1 AND task_type='DIAGNOSIS' AND status='FAILED'`, episodeID).Scan(&failed); err != nil {
			return err
		}
		if failed == 0 {
			return errCode("NOTHING_TO_RETRY", "no failed diagnosis task", 409)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE system.episode_input_versions
			SET queue_status='PENDING', assigned_task_id=NULL
			WHERE episode_id=$1 AND input_version > $2 AND queue_status <> 'PROCESSED'`,
			episodeID, ep.ProcessedInputVersion); err != nil {
			return err
		}
		before := ep.ProcessedInputVersion
		if err := s.ensureDiagnosisTask(ctx, tx, ep, s.now()); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `
			SELECT task_id FROM system.agent_tasks
			WHERE episode_id=$1 AND status='PENDING' AND task_type='DIAGNOSIS'
			ORDER BY created_at DESC LIMIT 1`, episodeID).Scan(&id)
		if err == sql.ErrNoRows {
			return errCode("NOTHING_TO_RETRY", "no unprocessed input version", 409)
		}
		_ = before
		return err
	})
	return id, err
}

// RetryArchive 只重建归档任务。
func (s *Store) RetryArchive(ctx context.Context, episodeID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var serviceID uuid.UUID
		if err := tx.QueryRowContext(ctx, `SELECT service_id FROM system.episodes WHERE episode_id=$1`, episodeID).Scan(&serviceID); err != nil {
			return err
		}
		if err := lockService(ctx, tx, serviceID); err != nil {
			return err
		}
		ep, err := lockEpisode(ctx, tx, episodeID)
		if err != nil {
			return err
		}
		if ep.Status != EpisodeResolved {
			return errCode("EPISODE_NOT_RESOLVED", "archive retry requires a resolved episode", 409)
		}
		active, err := countActiveTasks(ctx, tx, ep.ID)
		if err != nil {
			return err
		}
		if active > 0 {
			return errCode("TASK_ACTIVE", "an agent task is already active", 409)
		}
		if err := s.insertTask(ctx, tx, ep, TaskArchive, 0, 0, ep.FinalInputVersion, 1, s.now()); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			SELECT task_id FROM system.agent_tasks
			WHERE episode_id=$1 AND task_type='ARCHIVE' AND status='PENDING'
			ORDER BY created_at DESC LIMIT 1`, episodeID).Scan(&id)
	})
	return id, err
}

// OutboxBatch 是待发布的一批事件。
type OutboxMessage struct {
	OutboxID uuid.UUID
	Payload  []byte
}

func (s *Store) ClaimOutbox(ctx context.Context, limit int) ([]OutboxMessage, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT outbox_id, payload FROM system.outbox_events
		WHERE published_at IS NULL AND next_retry_at <= $1
		ORDER BY created_at
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, s.now(), limit)
	if err != nil {
		return nil, err
	}
	var msgs []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(&m.OutboxID, &m.Payload); err != nil {
			rows.Close()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return msgs, nil
}

// 领取后立即提交会丢掉锁。发布必须在同一事务里完成，所以 ClaimOutbox 只用于测试观察。
// Publisher 使用 PublishPending，在持锁期间写 Kafka 再标记。

func (s *Store) MarkPublished(ctx context.Context, outboxID uuid.UUID) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE system.outbox_events SET published_at=$2 WHERE outbox_id=$1 AND published_at IS NULL`, outboxID, s.now())
	return err
}

func (s *Store) MarkPublishFailed(ctx context.Context, outboxID uuid.UUID) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE system.outbox_events
		SET publish_attempts=publish_attempts+1, next_retry_at=$2
		WHERE outbox_id=$1 AND published_at IS NULL`, outboxID, s.now().Add(s.RetryBase))
	return err
}
