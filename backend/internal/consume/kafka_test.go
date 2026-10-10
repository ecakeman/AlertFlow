package consume

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/event"
	"github.com/ecakeman/alertflow/internal/publish"
)

func TestKafkaPublishAndConsume(t *testing.T) {
	brokers := os.Getenv("ALERTFLOW_KAFKA_BROKERS")
	if brokers == "" {
		t.Fatal("ALERTFLOW_KAFKA_BROKERS is required")
	}
	database := db.OpenTest(t)
	if _, err := database.CreateIntegration("webhook", "hook", true, false, json.RawMessage(`{"url":"http://example.invalid","token":"super-secret-token"}`)); err != nil {
		t.Fatal(err)
	}
	ev, err := event.NewCheckCompleted("svc", "API", "https://example.com/health", time.Now().Unix(), db.StatusOffline, nil, nil, "connection timeout", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordCheckAndOutbox(ev); err != nil {
		t.Fatal(err)
	}

	topic := "alert-events-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	dead, err := publish.New(database, "127.0.0.1:1", topic, "dead")
	if err != nil {
		t.Fatal(err)
	}
	deadCtx, cancelDead := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDead()
	if _, err := dead.PublishOnce(deadCtx); err == nil {
		t.Fatal("publish to a stopped broker succeeded")
	}
	stored, err := database.GetOutboxByEventID(ev.EventID)
	if err != nil || stored == nil || stored.PublishedAt != nil {
		t.Fatalf("outbox after broker failure: %+v %v", stored, err)
	}

	pub, err := publish.New(database, brokers, topic, "publisher-test")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := pub.PublishOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("publish = %d, %v", n, err)
	}
	stored, err = database.GetOutboxByEventID(ev.EventID)
	if err != nil || stored == nil || stored.PublishedAt == nil {
		t.Fatalf("published_at = %+v %v", stored, err)
	}

	cons, err := New(database, brokers, topic, "group-"+topic)
	if err != nil {
		t.Fatal(err)
	}
	if err := cons.ConsumeOne(ctx); err != nil {
		t.Fatal(err)
	}
	ongoing, err := database.GetOngoingIncident("svc")
	if err != nil || ongoing == nil {
		t.Fatalf("incident = %+v %v", ongoing, err)
	}
	var deliveries int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $1 AND status = 'pending' AND event_type = 'incident_start'`,
		ev.EventID,
	).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("pending deliveries = %d, %v", deliveries, err)
	}
	var payload string
	if err := database.QueryRow(`SELECT payload::text FROM notification_deliveries WHERE event_id = $1`, ev.EventID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "super-secret-token") {
		t.Fatalf("payload contains the integration secret: %s", payload)
	}
}
