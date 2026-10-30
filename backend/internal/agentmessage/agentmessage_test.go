package agentmessage

// 这些测试先于实现编写（B1）：锁定 builder 的字段语义、错误文案与裁剪顺序，
// 重构后由 notify 包 TestAgentMessageGoldenBytes 逐字节比对旧实现输出。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ecakeman/alertflow/internal/query"
)

var testEvent = time.Unix(1_700_000_000, 0)

func TestBuildDiagnosticWarningHasNoContext(t *testing.T) {
	msg := BuildDiagnostic(nil, DiagnosticInput{
		Event:       EventWarning,
		ServiceID:   "svc",
		ServiceName: "API",
		ServiceURL:  "https://example.com/health",
		OccurredAt:  testEvent,
		Attempts:    2,
		Error:       "timeout",
		DeliveryID:  7,
		SentAt:      testEvent.Add(123 * time.Second),
	})
	if msg.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %q", msg.SchemaVersion)
	}
	if msg.MessageType != MsgWarning {
		t.Fatalf("message_type = %q, want %q", msg.MessageType, MsgWarning)
	}
	if msg.CorrelationID != "delivery:7" {
		t.Fatalf("correlation_id = %q", msg.CorrelationID)
	}
	if msg.Payload.DiagnosticContext != nil {
		t.Fatalf("a warning must not carry a diagnostic context")
	}
	if msg.Payload.Event.Status != "warning" || msg.Payload.Event.Severity != "warning" {
		t.Fatalf("event = %+v", msg.Payload.Event)
	}
	if msg.Payload.Event.OccurredAt != "2023-11-14T22:13:20Z" || msg.SentAt != "2023-11-14T22:15:23Z" {
		t.Fatalf("timestamps = %q / %q", msg.Payload.Event.OccurredAt, msg.SentAt)
	}
}

func TestBuildDiagnosticWithoutIncidentIsATest(t *testing.T) {
	msg := BuildDiagnostic(nil, DiagnosticInput{
		Event:      EventIncidentStart,
		ServiceID:  "svc",
		OccurredAt: testEvent,
		StartedAt:  testEvent,
		DeliveryID: 9,
		SentAt:     testEvent,
	})
	if msg.MessageType != MsgTest {
		t.Fatalf("message_type = %q, want %q", msg.MessageType, MsgTest)
	}
	if msg.Payload.Event.Status != "down" || msg.Payload.Event.Severity != "outage" {
		t.Fatalf("event = %+v", msg.Payload.Event)
	}
	if msg.Payload.Event.EventID != "delivery:9" || msg.MessageID != "delivery:9" {
		t.Fatalf("ids = %q / %q", msg.Payload.Event.EventID, msg.MessageID)
	}
}

func TestBuildDiagnosticResolveAnchorsOnResolvedAt(t *testing.T) {
	resolved := testEvent.Add(5 * time.Minute)
	msg := BuildDiagnostic(nil, DiagnosticInput{
		Event:      EventIncidentResolve,
		IncidentID: 42,
		ServiceID:  "svc",
		OccurredAt: testEvent,
		StartedAt:  testEvent,
		ResolvedAt: &resolved,
		SentAt:     resolved,
	})
	if msg.MessageType != MsgResolved {
		t.Fatalf("message_type = %q", msg.MessageType)
	}
	if msg.MessageID != "incident_resolve:42" || msg.CorrelationID != "incident:42" {
		t.Fatalf("ids = %q / %q", msg.MessageID, msg.CorrelationID)
	}
	if msg.Payload.Event.ResolvedAt != "2023-11-14T22:18:20Z" {
		t.Fatalf("resolved_at = %q", msg.Payload.Event.ResolvedAt)
	}
	if msg.Payload.Event.Status != "recovered" {
		t.Fatalf("status = %q", msg.Payload.Event.Status)
	}
}

func TestBuildChatDefaultsAndClips(t *testing.T) {
	long := strings.Repeat("问", MaxChatMessageRunes+10)
	msg := BuildChat(ChatInput{
		RequestID: "req-1", IncidentID: 42, ServiceID: "svc",
		Message: long, SentAt: testEvent,
	})
	if msg.MessageID != "chat:req-1" || msg.MessageType != MsgChatRequested {
		t.Fatalf("chat ids = %q / %q", msg.MessageID, msg.MessageType)
	}
	if msg.CorrelationID != "incident:42" {
		t.Fatalf("correlation_id = %q", msg.CorrelationID)
	}
	if !utf8.ValidString(msg.Payload.Message) || utf8.RuneCountInString(msg.Payload.Message) != MaxChatMessageRunes+1 {
		t.Fatalf("clipped message has %d runes", utf8.RuneCountInString(msg.Payload.Message))
	}
	if string(msg.Payload.History) != "[]" {
		t.Fatalf("history = %s", msg.Payload.History)
	}
}

func TestEncodeChatRejectsOversize(t *testing.T) {
	body := strings.Repeat("x", 4096)
	history := json.RawMessage(`"` + body + `"`)
	_, err := EncodeChat(ChatInput{RequestID: "req", IncidentID: 1, Message: "hi", History: history}, 1024)
	if err == nil || !strings.Contains(err.Error(), "chat request is") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeRequiresQueryService(t *testing.T) {
	_, err := Encode(nil, DiagnosticInput{Event: EventIncidentStart, IncidentID: 1}, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "query service is not configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeDiagnosticRejectsOversizeWithoutContext(t *testing.T) {
	msg := BuildDiagnostic(nil, DiagnosticInput{
		Event:      EventIncidentStart,
		ServiceID:  "svc",
		OccurredAt: testEvent,
		SentAt:     testEvent,
		Error:      strings.Repeat("e", 4096),
	})
	_, err := encodeMessage(msg, 1024)
	if err == nil || !strings.Contains(err.Error(), "event payload is") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeDiagnosticTrimsContextInOrder(t *testing.T) {
	comments := make([]query.CommentContext, 30)
	for i := range comments {
		comments[i] = query.CommentContext{ID: int64(i), Body: strings.Repeat("很长的备注", 200)}
	}
	dc := &query.DiagnosticContext{
		Incident:     &query.IncidentContext{ID: 1, Comments: comments},
		RecentChecks: make([]query.CheckContext, 60),
		Metrics:      query.MetricsContext{Series: make([]query.SeriesContext, 120)},
		Coverage:     query.Coverage{Complete: true},
	}
	msg := BuildDiagnostic(nil, DiagnosticInput{
		Event: EventIncidentStart, IncidentID: 1, ServiceID: "svc",
		OccurredAt: testEvent, SentAt: testEvent,
	})
	msg.Payload.DiagnosticContext = dc

	full, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// 只差一点点：第一步（comments>10 留后 10）就足够让 payload 满足上限。
	body, err := encodeMessage(msg, len(full)-1)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(body) > len(full)-1 {
		t.Fatalf("body is %d bytes", len(body))
	}
	if len(dc.Incident.Comments) != 10 {
		t.Fatalf("comments = %d, want 10", len(dc.Incident.Comments))
	}
	if len(dc.Coverage.Truncated) != 1 || dc.Coverage.Truncated[0] != "COMMENTS_TRIMMED_FOR_PAYLOAD" {
		t.Fatalf("truncated = %v", dc.Coverage.Truncated)
	}
	if dc.Coverage.Complete {
		t.Fatalf("trimmed context reported complete")
	}
}

func TestEncodeDiagnosticDoesNotCallTheQueryServiceForWarnings(t *testing.T) {
	// q 为 nil 但事件是 warning：不会构建上下文，编码应成功。
	_, err := Encode(nil, DiagnosticInput{
		Event: EventWarning, ServiceID: "svc", OccurredAt: testEvent, SentAt: testEvent,
	}, nil, 0)
	if err == nil {
		t.Fatalf("expected the unconfigured-query error (matches the old Encode)")
	}
	// 直接走 builder 则成功，证明 warning 不需要 query service。
	body, err := EncodeDiagnostic(nil, DiagnosticInput{
		Event: EventWarning, ServiceID: "svc", OccurredAt: testEvent, SentAt: testEvent,
	}, 0)
	if err != nil || len(body) == 0 {
		t.Fatalf("warning encode = %v", err)
	}
}
