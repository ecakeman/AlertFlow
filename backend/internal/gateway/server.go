package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/system"
	agentv1 "github.com/ecakeman/alertflow/gen/agent/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SystemServer 是 Python 回调与查询使用的 gRPC 服务。
type SystemServer struct {
	agentv1.UnimplementedSystemGatewayServiceServer
	Store *system.Store
}

func (s *SystemServer) GetEpisodeInput(ctx context.Context, req *agentv1.GetEpisodeInputRequest) (*agentv1.GetEpisodeInputResponse, error) {
	taskID, episodeID, err := parseIDs(req.GetTaskId(), req.GetEpisodeId())
	if err != nil {
		return nil, err
	}
	if req.GetTargetInputVersion() == 0 {
		return s.archiveInput(ctx, taskID, episodeID, req.GetExecutionGeneration())
	}
	snap, err := s.Store.EpisodeInput(ctx, taskID, episodeID, req.GetTargetInputVersion(), req.GetExecutionGeneration())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	proj, err := structFromJSON(snap.Projection)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var pointWM, eventWM int64
	if snap.PointWatermark.Valid {
		pointWM = snap.PointWatermark.Int64
	}
	if snap.EventWatermark.Valid {
		eventWM = snap.EventWatermark.Int64
	}
	return &agentv1.GetEpisodeInputResponse{
		EpisodeId: snap.EpisodeID.String(), ServiceId: snap.ServiceID.String(), ServiceName: snap.ServiceName,
		InputVersion: snap.InputVersion, InputHash: snap.InputHash, MonitorProjection: proj,
		MonitorPointWatermark: pointWM, AppEventWatermark: eventWM,
		EpisodeStartedAt: timestamppb.New(snap.StartedAt),
	}, nil
}

func (s *SystemServer) archiveInput(ctx context.Context, taskID, episodeID uuid.UUID, generation int64) (*agentv1.GetEpisodeInputResponse, error) {
	var serviceID uuid.UUID
	var name string
	var started time.Time
	var final sql.NullInt64
	err := s.Store.DB.QueryRowContext(ctx, `
		SELECT e.service_id, sv.name, e.started_at, e.final_input_version
		FROM system.agent_tasks t
		JOIN system.episodes e ON e.episode_id=t.episode_id
		JOIN system.services sv ON sv.service_id=e.service_id
		WHERE t.task_id=$1 AND t.episode_id=$2 AND t.execution_generation=$3 AND t.task_type='ARCHIVE'`,
		taskID, episodeID, generation).Scan(&serviceID, &name, &started, &final)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "archive task is not visible")
	}
	version := int64(0)
	if final.Valid {
		version = final.Int64
	}
	empty, _ := structpb.NewStruct(map[string]any{})
	return &agentv1.GetEpisodeInputResponse{
		EpisodeId: episodeID.String(), ServiceId: serviceID.String(), ServiceName: name,
		InputVersion: version, MonitorProjection: empty, EpisodeStartedAt: timestamppb.New(started),
	}, nil
}

func (s *SystemServer) QueryTimeline(ctx context.Context, req *agentv1.QueryTimelineRequest) (*agentv1.QueryTimelineResponse, error) {
	taskID, episodeID, err := parseIDs(req.GetTaskId(), req.GetEpisodeId())
	if err != nil {
		return nil, err
	}
	if req.GetStartTime() == nil || req.GetEndTime() == nil {
		return nil, status.Error(codes.InvalidArgument, "start_time and end_time are required")
	}
	records, err := s.Store.QueryTimeline(ctx, taskID, episodeID, req.GetTargetInputVersion(), req.GetExecutionGeneration(),
		req.GetStartTime().AsTime(), req.GetEndTime().AsTime(), req.GetMonitorPointIds(), req.GetAppEventIds())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	out := &agentv1.QueryTimelineResponse{EpisodeId: req.GetEpisodeId(), TargetInputVersion: req.GetTargetInputVersion()}
	for _, rec := range records {
		content, err := structFromJSON(rec.Content)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		out.Records = append(out.Records, &agentv1.TimelineRecord{
			RecordType: rec.RecordType, RecordId: rec.RecordID,
			EventTime: timestamppb.New(rec.EventTime), ReceivedAt: timestamppb.New(rec.ReceivedAt),
			Source: rec.Source, Content: content,
		})
	}
	return out, nil
}

func (s *SystemServer) RegisterArtifact(ctx context.Context, req *agentv1.RegisterArtifactRequest) (*agentv1.RegisterArtifactResponse, error) {
	taskID, episodeID, err := parseIDs(req.GetTaskId(), req.GetEpisodeId())
	if err != nil {
		return nil, err
	}
	ok, version, err := s.Store.RegisterArtifact(ctx, system.RegisterArtifactInput{
		TaskID: taskID, EpisodeID: episodeID, ArtifactType: artifactName(req.GetArtifactType()),
		Generation: req.GetExecutionGeneration(), Version: req.GetArtifactVersion(),
		StorageKey: req.GetStorageKey(), ContentType: req.GetContentType(), ContentHash: req.GetContentHash(),
	})
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &agentv1.RegisterArtifactResponse{Registered: ok, ArtifactVersion: version}, nil
}

func (s *SystemServer) ReportTaskStarted(ctx context.Context, req *agentv1.ReportTaskStartedRequest) (*agentv1.TaskAck, error) {
	return s.ack(s.Store.ReportTaskStarted(ctx, mustID(req.GetTaskId()), mustID(req.GetEpisodeId()), req.GetExecutionGeneration()))
}

func (s *SystemServer) HeartbeatTask(ctx context.Context, req *agentv1.HeartbeatTaskRequest) (*agentv1.TaskAck, error) {
	return s.ack(s.Store.HeartbeatTask(ctx, mustID(req.GetTaskId()), mustID(req.GetEpisodeId()), req.GetExecutionGeneration()))
}

func (s *SystemServer) CompleteTask(ctx context.Context, req *agentv1.CompleteTaskRequest) (*agentv1.TaskAck, error) {
	ack, err := s.Store.CompleteTask(ctx, system.CompleteInput{
		TaskID: mustID(req.GetTaskId()), EpisodeID: mustID(req.GetEpisodeId()),
		TaskType: taskName(req.GetTaskType()), Generation: req.GetExecutionGeneration(),
		Outcome: req.GetOutcome(), ProcessedInputVersion: req.GetProcessedInputVersion(),
		FDID: req.GetFdId(), ErrorCode: req.GetErrorCode(), ErrorMessage: req.GetErrorMessage(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentv1.TaskAck{Accepted: ack.Accepted, TaskId: req.GetTaskId(), Message: ack.Message}, nil
}

func (s *SystemServer) ack(ack system.TaskAck, err error) (*agentv1.TaskAck, error) {
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentv1.TaskAck{Accepted: ack.Accepted, TaskId: ack.TaskID.String(), Message: ack.Message}, nil
}

func parseIDs(task, episode string) (uuid.UUID, uuid.UUID, error) {
	taskID, err := uuid.Parse(task)
	if err != nil {
		return uuid.Nil, uuid.Nil, status.Error(codes.InvalidArgument, "task_id")
	}
	episodeID, err := uuid.Parse(episode)
	if err != nil {
		return uuid.Nil, uuid.Nil, status.Error(codes.InvalidArgument, "episode_id")
	}
	return taskID, episodeID, nil
}

func mustID(raw string) uuid.UUID {
	id, _ := uuid.Parse(raw)
	return id
}

func taskName(t agentv1.AgentTaskType) string {
	if t == agentv1.AgentTaskType_AGENT_TASK_TYPE_ARCHIVE {
		return system.TaskArchive
	}
	return system.TaskDiagnosis
}

func artifactName(t agentv1.ArtifactType) string {
	if t == agentv1.ArtifactType_ARTIFACT_TYPE_DIAGNOSIS_REPORT {
		return "DIAGNOSIS_REPORT"
	}
	return "INVESTIGATION"
}

func structFromJSON(raw []byte) (*structpb.Struct, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	switch typed := v.(type) {
	case map[string]any:
		return structpb.NewStruct(typed)
	default:
		return structpb.NewStruct(map[string]any{"items": typed})
	}
}

// Notice 是一批输入版本里的变化引用。
type Notice struct {
	Version   int64
	Hash      string
	PointIDs  []string
	EventIDs  []string
	CreatedAt time.Time
}
