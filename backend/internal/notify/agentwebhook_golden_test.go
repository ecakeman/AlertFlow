package notify

// 本文件锁定 Agent v2 envelope 的字节输出，作为 B1 抽取（agentmessage）
// 前后等价性的回归证据。黄金文件由重构前的 notify.AgentSender.Encode 在固定
// 时钟下捕获（见 testdata/agentv2/README.md）；重构后由共享 builder 重建后
// 逐字节比较，并额外校验 HTTP 路径实际 POST 的 body 与之一致。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/agentmessage"
	"github.com/ecakeman/alertflow/internal/db"
)

// goldenClock 固定 sent_at，避免黄金文件随运行时间变化。
var goldenClock = time.Unix(1_700_000_123, 0)

func pinAgentClock(t *testing.T) {
	t.Helper()
	previous := agentClock
	agentClock = func() time.Time { return goldenClock }
	t.Cleanup(func() { agentClock = previous })
}

// encodeGolden 返回当前实现对一条消息的编码结果。
// 重构前调用旧的 AgentSender.Encode；B1 抽取后改为调用共享 builder，
// 黄金文件因此同时证明两条路径的字节一致。
func encodeGolden(t *testing.T, sender *AgentSender, msg Message) []byte {
	t.Helper()
	diag, chat := AgentMessageInputs(msg)
	raw, err := agentmessage.Encode(sender.Q, diag, chat, sender.MaxBytes)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

type goldenCase struct {
	name string
	// maxBytes 覆盖 sender.MaxBytes（0 表示默认 512KiB）。
	maxBytes int
	build    func(t *testing.T, database *db.DB) Message
}

func goldenCases() []goldenCase {
	event := time.Unix(1_700_000_000, 0)
	service := func(msg *Message) *Message {
		msg.ServiceID = "svc"
		msg.ServiceName = "API"
		msg.ServiceURL = "https://example.com/health"
		return msg
	}
	// withIncident 写入一条 check 与一个 ongoing incident，使诊断上下文非空。
	withIncident := func(t *testing.T, database *db.DB) int64 {
		t.Helper()
		if err := database.InsertCheck("svc", event.Unix(), db.StatusOffline, nil, nil, "connection refused", 3); err != nil {
			t.Fatalf("insert check: %v", err)
		}
		inc, err := database.CreateIncident("svc", "auto", event.Unix(), nil, nil)
		if err != nil {
			t.Fatalf("create incident: %v", err)
		}
		return inc.ID
	}
	return []goldenCase{
		{
			name: "warning-no-incident",
			build: func(t *testing.T, database *db.DB) Message {
				msg := Message{
					Event: EventWarning, OccurredAt: event, Attempts: 2,
					Error: "timeout", DeliveryID: 7,
				}
				return *service(&msg)
			},
		},
		{
			name: "test-no-incident",
			build: func(t *testing.T, database *db.DB) Message {
				msg := Message{
					Event: EventIncidentStart, OccurredAt: event, StartedAt: event, DeliveryID: 9,
				}
				return *service(&msg)
			},
		},
		{
			name: "incident-start-with-context",
			build: func(t *testing.T, database *db.DB) Message {
				msg := Message{
					Event: EventIncidentStart, IncidentID: withIncident(t, database),
					OccurredAt: event, StartedAt: event, CheckID: 1,
				}
				return *service(&msg)
			},
		},
		{
			name: "incident-resolve-with-context",
			build: func(t *testing.T, database *db.DB) Message {
				resolved := event.Add(5 * time.Minute)
				msg := Message{
					Event: EventIncidentResolve, IncidentID: withIncident(t, database),
					OccurredAt: resolved, StartedAt: event, ResolvedAt: &resolved, CheckID: 1,
				}
				return *service(&msg)
			},
		},
		{
			name: "chat-with-history",
			build: func(t *testing.T, database *db.DB) Message {
				msg := Message{
					Event: EventChatRequested, IncidentID: withIncident(t, database),
					OccurredAt: event,
					Chat: &ChatRequest{
						RequestID: "req-1",
						Message:   "为什么连接被拒绝？",
						History:   json.RawMessage(`[{"role":"user","content":"hi"}]`),
					},
				}
				return *service(&msg)
			},
		},
		{
			name: "chat-empty-history",
			build: func(t *testing.T, database *db.DB) Message {
				msg := Message{
					Event: EventChatRequested, IncidentID: withIncident(t, database),
					OccurredAt: event,
					Chat:       &ChatRequest{RequestID: "req-2", Message: "还在吗？"},
				}
				return *service(&msg)
			},
		},
		{
			// 超限场景，验证裁剪顺序与 coverage.truncated 的字节输出。
			name:     "trimmed-oversize-context",
			maxBytes: 8 << 10,
			build: func(t *testing.T, database *db.DB) Message {
				for i := 0; i < 40; i++ {
					if err := database.InsertCheck("svc", event.Unix()-int64(40-i), db.StatusOnline, nil, nil, "", 1); err != nil {
						t.Fatalf("insert check: %v", err)
					}
				}
				inc, err := database.CreateIncident("svc", "auto", event.Unix(), nil, nil)
				if err != nil {
					t.Fatalf("create incident: %v", err)
				}
				for i := 0; i < 30; i++ {
					if _, err := database.AddIncidentComment(inc.ID, nil, "很长的备注很长的备注很长的备注", event.Unix()); err != nil {
						t.Fatalf("add comment: %v", err)
					}
				}
				msg := Message{
					Event: EventIncidentStart, IncidentID: inc.ID,
					OccurredAt: event, StartedAt: event, CheckID: 1,
				}
				return *service(&msg)
			},
		},
	}
}

// TestAgentMessageGoldenBytes 比较重构后的编码与黄金文件是否逐字节一致。
// 设置 ALERTFLOW_UPDATE_GOLDEN=1 时会重写黄金文件（仅限捕获基线时使用）。
func TestAgentMessageGoldenBytes(t *testing.T) {
	update := os.Getenv("ALERTFLOW_UPDATE_GOLDEN") == "1"
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			pinAgentClock(t)
			sender, integration, database, bodyPtr, _ := agentFixture(t)
			sender.MaxBytes = tc.maxBytes
			msg := tc.build(t, database)

			got := encodeGolden(t, sender, msg)

			path := filepath.Join("testdata", "agentv2", tc.name+".json")
			if update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				t.Logf("wrote %s (%d bytes)", path, len(got))
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (re-run with ALERTFLOW_UPDATE_GOLDEN=1): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("encoded message differs from golden\n got: %s\nwant: %s", got, want)
			}

			// HTTP 路径（Send）实际发出的 body 必须与黄金字节一致。
			if err := sender.Send(context.Background(), integration, msg); err != nil {
				t.Fatalf("send: %v", err)
			}
			if !bytes.Equal(*bodyPtr, want) {
				t.Fatalf("HTTP body differs from golden\n got: %s\nwant: %s", *bodyPtr, want)
			}
		})
	}
}
