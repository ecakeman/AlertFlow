package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/system"
	agentv1 "github.com/ecakeman/alertflow/gen/agent/v1"
	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RunPublisher 把未发布 Outbox 写到 Kafka。
func RunPublisher(ctx context.Context, store *system.Store, brokers []string, topic string, log *slog.Logger) {
	writer := &kafka.Writer{
		Addr: kafka.TCP(brokers...), Topic: topic, Balancer: &kafka.Hash{},
		RequiredAcks: kafka.RequireOne, AllowAutoTopicCreation: true,
	}
	defer writer.Close()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := store.PublishPending(ctx, 32, func(payload []byte) error {
				var body struct {
					EpisodeID string `json:"episode_id"`
				}
				_ = json.Unmarshal(payload, &body)
				return writer.WriteMessages(ctx, kafka.Message{Key: []byte(body.EpisodeID), Value: payload})
			})
			if err != nil && log != nil {
				log.Error("publish outbox", "err", err)
			}
		}
	}
}

// RunConsumer 按 task_id 幂等调用 SubmitTask，成功后才提交 offset。
func RunConsumer(ctx context.Context, store *system.Store, brokers []string, topic, group string, target string, log *slog.Logger) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Error("agent grpc", "err", err)
		return
	}
	defer conn.Close()
	client := agentv1.NewAgentExecutionServiceClient(conn)
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers, Topic: topic, GroupID: group, MinBytes: 1, MaxBytes: 10e6,
	})
	defer reader.Close()
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		for {
			if ctx.Err() != nil {
				return
			}
			if err := deliver(ctx, store, client, msg.Value); err != nil {
				if log != nil {
					log.Error("deliver task", "err", err)
				}
				time.Sleep(time.Second)
				continue
			}
			break
		}
		if err := reader.CommitMessages(ctx, msg); err != nil && log != nil {
			log.Error("commit offset", "err", err)
		}
	}
}

func deliver(ctx context.Context, store *system.Store, client agentv1.AgentExecutionServiceClient, payload []byte) error {
	var body struct {
		TaskID              string `json:"task_id"`
		EpisodeID           string `json:"episode_id"`
		TaskType            string `json:"task_type"`
		InputVersionFrom    *int64 `json:"input_version_from"`
		InputVersionTo      *int64 `json:"input_version_to"`
		ExecutionGeneration int64  `json:"execution_generation"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return err
	}
	taskID, err := uuid.Parse(body.TaskID)
	if err != nil {
		return err
	}
	gen, status, err := store.TaskGeneration(ctx, taskID)
	if err != nil {
		return err
	}
	if gen != body.ExecutionGeneration {
		return nil
	}
	if status != system.TaskPending && status != system.TaskDispatched && status != system.TaskRunning && status != system.TaskSucceeded {
		return nil
	}
	req := &agentv1.SubmitTaskRequest{
		TaskId: body.TaskID, EpisodeId: body.EpisodeID, ExecutionGeneration: body.ExecutionGeneration, SchemaVersion: 1,
		TaskType: agentv1.AgentTaskType_AGENT_TASK_TYPE_DIAGNOSIS,
	}
	if body.TaskType == system.TaskArchive {
		req.TaskType = agentv1.AgentTaskType_AGENT_TASK_TYPE_ARCHIVE
	}
	if body.InputVersionFrom != nil {
		req.InputVersionFrom = *body.InputVersionFrom
	}
	if body.InputVersionTo != nil {
		req.InputVersionTo = *body.InputVersionTo
	}
	notices, err := loadNotices(ctx, store, taskID)
	if err != nil {
		return err
	}
	req.Notifications = notices
	resp, err := client.SubmitTask(ctx, req)
	if err != nil {
		return err
	}
	if !resp.GetAccepted() && !resp.GetDuplicate() {
		return errString(resp.GetMessage())
	}
	return store.MarkDispatched(ctx, taskID, body.ExecutionGeneration)
}

func loadNotices(ctx context.Context, store *system.Store, taskID uuid.UUID) ([]*agentv1.InputVersionNotice, error) {
	rows, err := store.DB.QueryContext(ctx, `
		SELECT input_version, input_hash, changed_refs, created_at
		FROM system.episode_input_versions WHERE assigned_task_id=$1 ORDER BY input_version`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*agentv1.InputVersionNotice
	for rows.Next() {
		var version int64
		var hash string
		var refs []byte
		var created time.Time
		if err := rows.Scan(&version, &hash, &refs, &created); err != nil {
			return nil, err
		}
		var changed struct {
			MonitorPointIDs []string `json:"monitor_point_ids"`
			AppEventIDs     []string `json:"app_event_ids"`
		}
		_ = json.Unmarshal(refs, &changed)
		out = append(out, &agentv1.InputVersionNotice{
			InputVersion: version, InputHash: hash,
			ChangedMonitorPointIds: changed.MonitorPointIDs, ChangedAppEventIds: changed.AppEventIDs,
			CreatedAt: timestamppb.New(created),
		})
	}
	return out, rows.Err()
}

type errString string

func (e errString) Error() string { return string(e) }

// RunRecovery 周期执行租约、重试和缺任务扫描。
func RunRecovery(ctx context.Context, store *system.Store, every time.Duration) {
	if every <= 0 {
		every = 2 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = store.RecoverOnce(ctx)
		}
	}
}
