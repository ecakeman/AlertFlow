package db

import (
	"errors"
	"testing"
)

func TestRetiredChannelIsNotClaimedOrSwept(t *testing.T) {
	database := OpenTest(t)
	in, err := database.CreateIntegration("webhook", "old", true, false, []byte(`{"url":"http://example.invalid"}`))
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1_000)
	if _, err := database.Exec(
		`INSERT INTO notification_deliveries (
			event_id, integration_id, event_type, payload, status,
			attempt_count, next_attempt_at, created_at, updated_at
		) VALUES ('evt-old', $1, 'incident_start', '{}', 'pending', 5, $2, $2, $2)`,
		in.ID, now,
	); err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimDeliveries("worker", now, now+30, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed retired rows: %+v", claimed)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM notification_deliveries WHERE event_id = 'evt-old'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("status = %s, want pending", status)
	}
}

func TestCancelRetiredDeliveriesLeavesRunning(t *testing.T) {
	database := OpenTest(t)
	slack, err := database.CreateIntegration("slack", "ops", true, false, []byte(`{"webhookUrl":"https://example.invalid"}`))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := database.CreateIntegration(AgentWebhookType, "agent", true, false, []byte(`{"url":"http://example.invalid/agent"}`))
	if err != nil {
		t.Fatal(err)
	}
	now := int64(2_000)
	insert := func(eventID string, integrationID int64, status string) {
		t.Helper()
		if _, err := database.Exec(
			`INSERT INTO notification_deliveries (
				event_id, integration_id, event_type, payload, status,
				attempt_count, next_attempt_at, lease_owner, created_at, updated_at
			) VALUES ($1, $2, 'incident_start', '{}', $3, 1, $4, 'old-worker', $4, $4)`,
			eventID, integrationID, status, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	insert("pending-slack", slack.ID, "pending")
	insert("retry-slack", slack.ID, "retry_wait")
	insert("running-slack", slack.ID, "running")
	insert("pending-agent", agent.ID, "pending")

	first, err := database.CancelRetiredDeliveries(now + 10)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cancelled != 2 || first.Audited != 1 {
		t.Fatalf("result = %+v", first)
	}
	second, err := database.CancelRetiredDeliveries(now + 20)
	if err != nil {
		t.Fatal(err)
	}
	if second.Cancelled != 0 || second.Audited != 0 {
		t.Fatalf("second result = %+v", second)
	}

	statusOf := func(eventID string) string {
		t.Helper()
		var status string
		if err := database.QueryRow(`SELECT status FROM notification_deliveries WHERE event_id = $1`, eventID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if statusOf("pending-slack") != "cancelled" || statusOf("retry-slack") != "cancelled" {
		t.Fatalf("queued = %s %s", statusOf("pending-slack"), statusOf("retry-slack"))
	}
	if statusOf("running-slack") != "running" || statusOf("pending-agent") != "pending" {
		t.Fatalf("running=%s agent=%s", statusOf("running-slack"), statusOf("pending-agent"))
	}
	var reason string
	if err := database.QueryRow(`SELECT last_error FROM notification_deliveries WHERE event_id = 'pending-slack'`).Scan(&reason); err != nil || reason == "" {
		t.Fatalf("cancel reason = %q, %v", reason, err)
	}
	var audits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM retired_delivery_audits WHERE reason <> ''`).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("audits = %d, %v", audits, err)
	}

	var id int64
	if err := database.QueryRow(`SELECT id FROM notification_deliveries WHERE event_id = 'pending-slack'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := database.RetryDeadDelivery(id, now); !errors.Is(err, ErrChannelRetired) && !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("retry cancelled = %v", err)
	}
	if _, err := database.Exec(`UPDATE notification_deliveries SET status = 'dead' WHERE event_id = 'pending-slack'`); err != nil {
		t.Fatal(err)
	}
	if err := database.RetryDeadDelivery(id, now); !errors.Is(err, ErrChannelRetired) {
		t.Fatalf("retry dead retired = %v", err)
	}
	if err := database.DeleteIntegration(slack.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM retired_delivery_audits WHERE integration_id = $1`, slack.ID).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("audits after delete = %d, %v", audits, err)
	}
}
