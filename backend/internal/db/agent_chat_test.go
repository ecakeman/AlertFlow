package db

import (
	"encoding/json"
	"testing"
)

func chatFixture(t *testing.T) (*DB, int64) {
	t.Helper()
	database := OpenTest(t)
	inc, err := database.CreateIncident("svc", "auto", 1_700_000_000, nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	return database, inc.ID
}

func TestCreateChatRequestQueuesOneDeliveryAndDedupes(t *testing.T) {
	database, incidentID := chatFixture(t)
	if _, err := database.CreateIntegration(
		AgentWebhookType, "agent", true, false, json.RawMessage(`{"url":"http://agent.invalid"}`),
	); err != nil {
		t.Fatalf("create integration: %v", err)
	}

	now := int64(1_700_000_000)
	payload := []byte(`{"event":"chat_requested","chat":{"requestId":"chat-1"}}`)
	rec := ChatRequestRow{RequestID: "chat-1", IncidentID: incidentID, Message: "why?"}

	row, created, err := database.CreateChatRequest(rec, payload, now)
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	if row.DeliveryID == nil {
		t.Fatalf("no delivery was queued: %+v", row)
	}
	if row.Status != ChatPending {
		t.Fatalf("status = %q", row.Status)
	}

	// A retried POST must not queue the question twice.
	again, created, err := database.CreateChatRequest(rec, payload, now)
	if err != nil {
		t.Fatalf("repeat create: %v", err)
	}
	if created {
		t.Fatalf("a retried request created a second row")
	}
	if again.RequestID != "chat-1" {
		t.Fatalf("returned row = %+v", again)
	}
	var deliveries int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $1`, "chat-1",
	).Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveries != 1 {
		t.Fatalf("queued %d deliveries, want 1", deliveries)
	}
	// The delivery must be addressed to the agent channel, not a notification one.
	var eventType string
	var integrationID *int64
	if err := database.QueryRow(
		`SELECT integration_id, event_type FROM notification_deliveries WHERE event_id = $1`, "chat-1",
	).Scan(&integrationID, &eventType); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if integrationID != nil || eventType != EventChatRequested {
		t.Fatalf("delivery integration=%v type=%s", integrationID, eventType)
	}
}

func TestCreateChatRequestWithoutAnAgentChannelFailsLoudly(t *testing.T) {
	database, incidentID := chatFixture(t)
	row, created, err := database.CreateChatRequest(
		ChatRequestRow{RequestID: "chat-x", IncidentID: incidentID, Message: "hi"},
		[]byte(`{"event":"chat_requested"}`), 1_700_000_000)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if row.DeliveryID == nil {
		t.Fatalf("internal task was not queued: %+v", row)
	}
}

func TestChatRequestLifecycle(t *testing.T) {
	database, incidentID := chatFixture(t)
	if _, err := database.CreateIntegration(
		AgentWebhookType, "agent", true, false, json.RawMessage(`{"url":"http://agent.invalid"}`),
	); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	if _, _, err := database.CreateChatRequest(
		ChatRequestRow{RequestID: "chat-2", IncidentID: incidentID, Message: "why?"},
		[]byte(`{}`), 1_700_000_000); err != nil {
		t.Fatalf("create: %v", err)
	}

	applied, err := database.CompleteChatRequest("chat-2", "because of the probe", json.RawMessage(`["chk:11"]`), 1_700_000_100)
	if err != nil || !applied {
		t.Fatalf("complete: applied=%v err=%v", applied, err)
	}
	// A redelivered answer changes nothing.
	applied, err = database.CompleteChatRequest("chat-2", "a different answer", nil, 1_700_000_200)
	if err != nil {
		t.Fatalf("repeat complete: %v", err)
	}
	if applied {
		t.Fatalf("a redelivered answer overwrote the stored one")
	}
	got, err := database.GetChatRequest("chat-2")
	if err != nil || got == nil {
		t.Fatalf("read back: %v %+v", err, got)
	}
	if got.Status != ChatAnswered || got.Reply == nil || *got.Reply != "because of the probe" {
		t.Fatalf("row = %+v", got)
	}
	var ids []string
	if err := json.Unmarshal(got.EvidenceIDs, &ids); err != nil || len(ids) != 1 || ids[0] != "chk:11" {
		t.Fatalf("evidence ids = %s (%v)", got.EvidenceIDs, err)
	}

	if _, err := database.FailChatRequest("chat-2", "too late", 1_700_000_300); err != nil {
		t.Fatalf("fail after answer: %v", err)
	}
	got, _ = database.GetChatRequest("chat-2")
	if got.Status != ChatAnswered {
		t.Fatalf("a late failure changed an answered request: %+v", got)
	}
}

func TestExpireStaleChatRequests(t *testing.T) {
	database, incidentID := chatFixture(t)
	old := int64(1_700_000_000)
	if _, _, err := database.CreateChatRequest(
		ChatRequestRow{RequestID: "chat-old", IncidentID: incidentID, Message: "?"}, []byte(`{}`), old); err != nil {
		t.Fatalf("create: %v", err)
	}
	fresh := old + int64(ChatExpireThreshold.Seconds()) + 1
	if _, _, err := database.CreateChatRequest(
		ChatRequestRow{RequestID: "chat-new", IncidentID: incidentID, Message: "?"}, []byte(`{}`), fresh); err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := database.ExpireStaleChatRequests(fresh)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}
	oldRow, _ := database.GetChatRequest("chat-old")
	if oldRow.Status != ChatFailed || oldRow.Error == "" {
		t.Fatalf("stale request = %+v", oldRow)
	}
	newRow, _ := database.GetChatRequest("chat-new")
	if newRow.Status != ChatPending {
		t.Fatalf("fresh request was expired: %+v", newRow)
	}
}
