package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/ecakeman/alertflow/gen/alertflow/agent/v1"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/query"
)

// ServeControl listens for Agent reads and callbacks. An empty token refuses
// every call before any row is written.
func (s *Server) ServeControl(ctx context.Context, addr, token string) error {
	if addr == "" {
		addr = ":9090"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	gs := grpc.NewServer(
		grpc.UnaryInterceptor(bearerInterceptor(token)),
		grpc.MaxRecvMsgSize(agentCallbackBodyLimit),
		grpc.MaxSendMsgSize(agentCallbackBodyLimit),
	)
	agentv1.RegisterAlertFlowControlServiceServer(gs, &controlServer{s: s})
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	slog.Info("agent grpc listening", "addr", addr)
	err = gs.Serve(lis)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

func bearerInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if token == "" {
			return nil, status.Error(codes.Unavailable, "agent grpc token is not configured")
		}
		md, _ := metadata.FromIncomingContext(ctx)
		got := ""
		if vals := md.Get("authorization"); len(vals) > 0 {
			got = vals[0]
		}
		const prefix = "Bearer "
		if len(got) < len(prefix) || !strings.EqualFold(got[:len(prefix)], prefix) {
			return nil, status.Error(codes.Unauthenticated, "unauthorized")
		}
		provided := got[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "unauthorized")
		}
		return handler(ctx, req)
	}
}

type controlServer struct {
	agentv1.UnimplementedAlertFlowControlServiceServer
	s *Server
}

func (c *controlServer) GetRuntimeConfig(context.Context, *agentv1.GetRuntimeConfigRequest) (*agentv1.RuntimeConfig, error) {
	a := c.s.config().Settings.Agent
	memory := true
	if a.MemoryEnabled != nil {
		memory = *a.MemoryEnabled
	}
	return &agentv1.RuntimeConfig{
		MemoryEnabled:       memory,
		ContextBudgetTokens: int32(a.ContextBudgetTokens),
		Llm:                 endpoint(a.LLM.BaseURL, a.LLM.APIKey, a.LLM.Model, 0, ""),
		Embed:               endpoint(a.Embed.BaseURL, a.Embed.APIKey, a.Embed.Model, int32(a.Embed.Dim), ""),
		Rerank:              endpoint(a.Rerank.BaseURL, a.Rerank.APIKey, a.Rerank.Model, 0, a.Rerank.Style),
	}, nil
}

func endpoint(base, key, model string, dim int32, style string) *agentv1.ModelEndpoint {
	return &agentv1.ModelEndpoint{BaseUrl: base, ApiKey: key, Model: model, Dim: dim, Style: style}
}

func (c *controlServer) ListIncidents(_ context.Context, req *agentv1.ListIncidentsRequest) (*agentv1.ListIncidentsResponse, error) {
	if req.GetStatus() != "" && req.GetStatus() != "ongoing" && req.GetStatus() != "resolved" {
		return nil, status.Error(codes.InvalidArgument, "status must be ongoing or resolved")
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = query.DefaultIncidentPageSize
	}
	if limit > query.MaxIncidentPageSize {
		limit = query.MaxIncidentPageSize
	}
	page, err := c.s.query().Incidents(query.IncidentFilter{
		ServiceID: req.GetServiceId(), Status: req.GetStatus(), Severity: req.GetSeverity(),
		Limit: limit, Offset: int(req.GetOffset()),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "could not load incidents")
	}
	out := &agentv1.ListIncidentsResponse{Total: int64(page.Total)}
	for _, inc := range page.Incidents {
		out.Incidents = append(out.Incidents, incidentProto(inc, c.s.serviceName(inc.ServiceID)))
	}
	return out, nil
}

func (c *controlServer) GetIncident(_ context.Context, req *agentv1.GetIncidentRequest) (*agentv1.IncidentDetail, error) {
	if req.GetId() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "incident id is required")
	}
	detail, err := c.s.query().IncidentDetail(req.GetId())
	if errors.Is(err, query.ErrIncidentNotFound) {
		return nil, status.Error(codes.NotFound, "incident not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "could not load incident")
	}
	out := &agentv1.IncidentDetail{Incident: incidentProto(detail.Incident, c.s.serviceName(detail.Incident.ServiceID))}
	for _, cm := range detail.Comments {
		out.Comments = append(out.Comments, &agentv1.IncidentComment{
			Id: cm.ID, Username: cm.Username, Body: cm.Body, CreatedAt: cm.CreatedAt,
		})
	}
	return out, nil
}

func (c *controlServer) GetMetrics(ctx context.Context, req *agentv1.GetMetricsRequest) (*agentv1.Metrics, error) {
	if req.GetServiceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	res, err := c.s.query().ServiceMetrics(ctx, req.GetServiceId(), req.GetRange(), time.Now())
	if errors.Is(err, query.ErrServiceNotFound) {
		return nil, status.Error(codes.NotFound, "service not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "could not load metrics")
	}
	out := &agentv1.Metrics{
		ServiceId: req.GetServiceId(), Name: res.Service.Name, Url: res.Service.URL,
		From: res.From, To: res.To, BucketSeconds: res.BucketSeconds,
		Uptime_7D: res.Uptime.Days7, Uptime_30D: res.Uptime.Days30, Uptime_365D: res.Uptime.Days365,
	}
	for _, pt := range res.Series {
		item := &agentv1.SeriesPoint{Ts: pt.Ts, Errors: int32(pt.Errors), Warnings: int32(pt.Warnings)}
		if pt.AvgLatency != nil {
			item.AvgLatency = *pt.AvgLatency
			item.HasAvgLatency = true
		}
		out.Series = append(out.Series, item)
	}
	return out, nil
}

func (c *controlServer) ListChecks(ctx context.Context, req *agentv1.ListChecksRequest) (*agentv1.ListChecksResponse, error) {
	if req.GetServiceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	page, err := c.s.query().CheckHistory(ctx, req.GetServiceId(), req.GetBefore(), int(req.GetLimit()))
	if errors.Is(err, query.ErrServiceNotFound) {
		return nil, status.Error(codes.NotFound, "service not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "could not load checks")
	}
	out := &agentv1.ListChecksResponse{}
	if page.NextBefore != nil {
		out.NextBefore = *page.NextBefore
	}
	for _, row := range page.Checks {
		item := &agentv1.Check{
			Id: row.ID, Ts: row.Ts, Status: row.Status, Error: row.Error, Attempts: int32(row.Attempts),
		}
		if row.LatencyMs != nil {
			item.LatencyMs = int32(*row.LatencyMs)
			item.HasLatencyMs = true
		}
		if row.StatusCode != nil {
			item.StatusCode = int32(*row.StatusCode)
			item.HasStatusCode = true
		}
		out.Checks = append(out.Checks, item)
	}
	return out, nil
}

func (c *controlServer) GetTimeline(_ context.Context, req *agentv1.GetTimelineRequest) (*agentv1.TimelinePage, error) {
	if req.GetServiceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	var from, to time.Time
	if req.GetFrom() > 0 {
		from = time.Unix(req.GetFrom(), 0).UTC()
	}
	if req.GetTo() > 0 {
		to = time.Unix(req.GetTo(), 0).UTC()
	}
	page, err := c.s.query().ServiceTimeline(req.GetServiceId(), query.TimelineRequest{
		From: from, To: to,
		Sources: req.GetSources(), Types: req.GetTypes(), Limit: int(req.GetLimit()),
		CursorAt: req.GetCursorAt(), CursorID: req.GetCursorId(),
		HasCursor: req.GetCursorAt() > 0 || req.GetCursorId() > 0,
	})
	if errors.Is(err, query.ErrServiceNotFound) {
		return nil, status.Error(codes.NotFound, "service not found")
	}
	if errors.Is(err, query.ErrTimelineFilter) || errors.Is(err, query.ErrTimelineRange) {
		return nil, status.Error(codes.InvalidArgument, "invalid timeline query")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "could not load timeline")
	}
	out := &agentv1.TimelinePage{
		NextCursorAt: page.NextCursorAt, NextCursorId: page.NextCursorID, HasNext: page.HasNext,
		From: page.From.Unix(), To: page.To.Unix(), RetentionDays: int32(page.RetentionDays),
		Coverage: coverageProto(page.Coverage),
	}
	for _, ev := range page.Events {
		item := &agentv1.TimelineEvent{
			Id: ev.ID, EventKey: ev.EventKey, ServiceId: ev.ServiceID, Source: ev.Source,
			EventType: ev.EventType, Severity: ev.Severity, Title: ev.Title, Summary: ev.Summary,
			OccurredAt: ev.OccurredAt, ObservedAt: ev.ObservedAt, RecordedAt: ev.RecordedAt,
			Details: rawStruct(ev.Details),
		}
		if ev.IncidentID.Valid {
			item.IncidentId = ev.IncidentID.Int64
			item.HasIncidentId = true
		}
		out.Events = append(out.Events, item)
	}
	return out, nil
}

func (c *controlServer) GetSourceCoverage(_ context.Context, req *agentv1.GetSourceCoverageRequest) (*agentv1.SourceCoverage, error) {
	page, err := c.GetTimeline(context.Background(), &agentv1.GetTimelineRequest{ServiceId: req.GetServiceId(), Limit: 1})
	if err != nil {
		return nil, err
	}
	return &agentv1.SourceCoverage{Sources: page.Coverage, RetentionDays: page.RetentionDays}, nil
}

func (c *controlServer) GetKubernetesState(_ context.Context, req *agentv1.GetKubernetesStateRequest) (*agentv1.KubernetesState, error) {
	if req.GetServiceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "service_id is required")
	}
	if c.s.query().Find(req.GetServiceId()) == nil {
		return nil, status.Error(codes.NotFound, "service not found")
	}
	rows, err := c.s.conn().ListKubernetesObjects(req.GetServiceId())
	if err != nil {
		return nil, status.Error(codes.Internal, "could not load kubernetes state")
	}
	out := &agentv1.KubernetesState{}
	for _, row := range rows {
		statusText := "inactive"
		if row.Active {
			statusText = "active"
		}
		out.Objects = append(out.Objects, &agentv1.KubernetesObject{
			ClusterId: row.ClusterID, Namespace: row.Namespace, Kind: row.Kind, Name: row.Name,
			Uid: row.UID, Active: row.Active, LastSeenAt: row.ObservedAt, Status: statusText,
		})
	}
	return out, nil
}

func (c *controlServer) SubmitAgentCallback(ctx context.Context, req *agentv1.AgentCallback) (*agentv1.CallbackResult, error) {
	if req.GetSchemaVersion() != "v2" {
		return nil, status.Error(codes.InvalidArgument, "schema_version must be v2")
	}
	if req.GetMessageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "message_id is required")
	}
	switch req.GetMessageType() {
	case "chat.completed", "chat.failed":
		return c.chatCallback(ctx, req)
	case "run.started", "run.completed", "run.failed":
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported message_type")
	}
	if req.GetRunId() == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}
	inserted, err := c.s.conn().SaveAgentRun(db.AgentRunRecord{
		MessageID: req.GetMessageId(), RunID: req.GetRunId(), MessageType: req.GetMessageType(),
		CorrelationID: req.GetCorrelationId(), IncidentID: optionalID(req.GetHasIncidentId(), req.GetIncidentId()),
		ServiceID: req.GetServiceId(), EventType: req.GetEventType(), Mode: req.GetMode(), Status: req.GetStatus(),
		Report: structRaw(req.GetReport()), Evidence: structRaw(req.GetEvidence()), Usage: structRaw(req.GetUsage()),
		Error: req.GetError(), StartedAt: optionalTime(req.GetStartedAt()), FinishedAt: optionalTime(req.GetFinishedAt()),
	})
	if err != nil {
		slog.ErrorContext(ctx, "agent grpc callback", "message_id", req.GetMessageId(), "error", err)
		return nil, status.Error(codes.Internal, "could not store the agent result")
	}
	return &agentv1.CallbackResult{Stored: inserted, Duplicate: !inserted}, nil
}

func (c *controlServer) chatCallback(ctx context.Context, req *agentv1.AgentCallback) (*agentv1.CallbackResult, error) {
	if req.GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required")
	}
	reason := ""
	if req.GetMessageType() == "chat.failed" {
		reason = strings.TrimSpace(req.GetError())
		if reason == "" {
			reason = "the agent could not answer"
		}
	}
	raw, _ := json.Marshal(req.GetEvidenceIds())
	applied, err := c.s.conn().ApplyChatCallback(req.GetMessageId(), req.GetMessageType(), req.GetRequestId(), req.GetReply(), raw, reason, time.Now().Unix())
	if err != nil {
		slog.ErrorContext(ctx, "agent grpc chat callback", "request_id", req.GetRequestId(), "error", err)
		return nil, status.Error(codes.Internal, "could not store the chat answer")
	}
	return &agentv1.CallbackResult{Stored: applied, Duplicate: !applied}, nil
}

func incidentProto(inc db.Incident, name string) *agentv1.Incident {
	out := &agentv1.Incident{
		Id: inc.ID, ServiceId: inc.ServiceID, ServiceName: name, Status: inc.Status,
		Source: inc.Source, Severity: inc.Severity, Title: inc.Title, StartedAt: inc.StartedAt,
		CreatedAt: inc.CreatedAt, UpdatedAt: inc.UpdatedAt,
	}
	if inc.ResolvedAt != nil {
		out.ResolvedAt = *inc.ResolvedAt
		out.HasResolvedAt = true
	}
	return out
}

func coverageProto(in map[string]query.TimelineCoverage) []*agentv1.SourceWindow {
	var out []*agentv1.SourceWindow
	for source, row := range in {
		item := &agentv1.SourceWindow{Source: source, Status: row.Status, LastError: row.LastError}
		if row.LastSuccess != nil {
			item.LastSuccessAt = *row.LastSuccess
			item.HasLastSuccessAt = true
		}
		out = append(out, item)
	}
	return out
}

func optionalID(ok bool, id int64) *int64 {
	if !ok || id == 0 {
		return nil
	}
	return &id
}

func optionalTime(sec int64) *int64 {
	if sec == 0 {
		return nil
	}
	return &sec
}

func structRaw(st *structpb.Struct) json.RawMessage {
	if st == nil {
		return nil
	}
	raw, err := protojson.Marshal(st)
	if err != nil {
		return nil
	}
	return raw
}

func rawStruct(raw json.RawMessage) *structpb.Struct {
	if len(raw) == 0 {
		return nil
	}
	st := &structpb.Struct{}
	if protojson.Unmarshal(raw, st) != nil {
		return nil
	}
	return st
}
