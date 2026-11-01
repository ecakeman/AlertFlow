package system

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PublishPending 在行锁内发送并标记 Outbox。发送成功但提交失败时，消费方按 task_id 去重。
func (s *Store) PublishPending(ctx context.Context, limit int, send func(payload []byte) error) (int, error) {
	if limit <= 0 {
		limit = 16
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT outbox_id, payload FROM system.outbox_events
		WHERE published_at IS NULL AND next_retry_at <= $1
		ORDER BY created_at
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, s.now(), limit)
	if err != nil {
		return 0, err
	}
	type item struct {
		id      uuid.UUID
		payload []byte
	}
	var batch []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.payload); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	published := 0
	now := s.now()
	for _, it := range batch {
		if err := send(it.payload); err != nil {
			if _, uerr := tx.ExecContext(ctx, `
				UPDATE system.outbox_events
				SET publish_attempts=publish_attempts+1, next_retry_at=$2
				WHERE outbox_id=$1`, it.id, now.Add(s.RetryBase)); uerr != nil {
				return published, uerr
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE system.outbox_events SET published_at=$2 WHERE outbox_id=$1`, it.id, now); err != nil {
			return published, err
		}
		published++
	}
	return published, tx.Commit()
}

// UnpublishedCount 给测试和恢复扫描看还有没有未发布事件。
func (s *Store) UnpublishedCount(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM system.outbox_events WHERE published_at IS NULL`).Scan(&n)
	return n, err
}

// TaskGeneration 返回任务当前代次。
func (s *Store) TaskGeneration(ctx context.Context, taskID uuid.UUID) (int64, string, error) {
	var gen int64
	var status string
	err := s.DB.QueryRowContext(ctx, `SELECT execution_generation, status FROM system.agent_tasks WHERE task_id=$1`, taskID).Scan(&gen, &status)
	return gen, status, err
}

// LeaseUntil 读取租约，供过期测试使用。
func (s *Store) ForceLeaseExpired(ctx context.Context, taskID uuid.UUID, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE system.agent_tasks SET status='RUNNING', lease_until=$2, started_at=COALESCE(started_at,$2) WHERE task_id=$1`,
		taskID, at)
	return err
}
