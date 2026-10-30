package deliver

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/ecakeman/alertflow/gen/alertflow/agent/v1"
	"github.com/ecakeman/alertflow/internal/agentmessage"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/notify"
	"github.com/ecakeman/alertflow/internal/query"
)

// GRPCTasks starts one investigation or chat on the Agent. The connection is
// reused. A missing target is not a client: the worker leaves the task pending.
type GRPCTasks struct {
	conn   *grpc.ClientConn
	client agentv1.AgentTaskServiceClient
	Q      *query.Service
}

func DialTasks(ctx context.Context, target, token string) (*GRPCTasks, error) {
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearerCreds{token: token}),
	)
	if err != nil {
		return nil, err
	}
	return &GRPCTasks{conn: conn, client: agentv1.NewAgentTaskServiceClient(conn)}, nil
}

func (g *GRPCTasks) Close() error {
	if g == nil || g.conn == nil {
		return nil
	}
	return g.conn.Close()
}

func (g *GRPCTasks) Accept(ctx context.Context, row db.ClaimedDelivery) error {
	msg, err := messageFrom(row.Payload)
	if err != nil {
		return &notify.StatusError{Code: 400, Body: "invalid task payload"}
	}
	msg.DeliveryID = row.ID
	// 与 HTTP 路径共用同一个 builder：gRPC 只是另一层 transport。
	diag, chat := notify.AgentMessageInputs(msg)
	raw, err := agentmessage.Encode(g.Q, diag, chat, 0)
	if err != nil {
		return err
	}
	var env struct {
		MessageID     string          `json:"message_id"`
		MessageType   string          `json:"message_type"`
		CorrelationID string          `json:"correlation_id"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return &notify.StatusError{Code: 400, Body: "invalid task payload"}
	}
	payload := &structpb.Struct{}
	if len(env.Payload) > 0 && string(env.Payload) != "null" {
		if err := protojson.Unmarshal(env.Payload, payload); err != nil {
			return &notify.StatusError{Code: 400, Body: "invalid task payload"}
		}
	}
	var resp *agentv1.TaskAccept
	if env.MessageType == notify.AgentMsgChatRequested {
		resp, err = g.client.StartChat(ctx, &agentv1.ChatRequest{
			MessageId:  env.MessageID,
			RequestId:  requestID(env.Payload, row.EventID),
			IncidentId: msg.IncidentID,
			Message:    chatText(msg),
			Payload:    payload,
		})
	} else {
		resp, err = g.client.StartInvestigation(ctx, &agentv1.InvestigationRequest{
			MessageId:     env.MessageID,
			MessageType:   env.MessageType,
			CorrelationId: env.CorrelationID,
			Payload:       payload,
		})
	}
	if err != nil {
		return grpcStatus(err)
	}
	if resp.GetAccepted() || resp.GetDuplicate() {
		return nil
	}
	return &notify.StatusError{Code: 500, Body: "agent did not accept the task"}
}

func chatText(msg notify.Message) string {
	if msg.Chat == nil {
		return ""
	}
	return msg.Chat.Message
}

func requestID(raw json.RawMessage, fallback string) string {
	var body struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &body) == nil && body.RequestID != "" {
		return body.RequestID
	}
	return fallback
}

func grpcStatus(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied, codes.NotFound:
		return &notify.StatusError{Code: 400, Body: "agent rejected the task"}
	default:
		return err
	}
}

type bearerCreds struct{ token string }

func (b bearerCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (bearerCreds) RequireTransportSecurity() bool { return false }
