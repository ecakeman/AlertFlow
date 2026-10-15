// Package consume reads check events and applies each one in one database
// transaction. It commits the Kafka offset only after that transaction commits.
// It does not send notifications; it only inserts pending delivery rows.
package consume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/event"
	"github.com/ecakeman/alertflow/internal/publish"
)

const groupID = "alertflow-incident"

// Consumer is the single incident consumer group member this phase runs.
type Consumer struct {
	DB      *db.DB
	Brokers []string
	Topic   string
	GroupID string
}

// New builds a consumer. An empty topic or group uses the production names.
func New(database *db.DB, brokers, topic, group string) (*Consumer, error) {
	list := splitBrokers(brokers)
	if len(list) == 0 {
		return nil, fmt.Errorf("kafka brokers are empty (set ALERTFLOW_KAFKA_BROKERS)")
	}
	if topic == "" {
		topic = publish.Topic
	}
	if group == "" {
		group = groupID
	}
	return &Consumer{DB: database, Brokers: list, Topic: topic, GroupID: group}, nil
}

func splitBrokers(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Run reads until ctx is cancelled. A database error leaves the offset uncommitted.
func (c *Consumer) Run(ctx context.Context) error {
	reader := c.reader()
	defer func() { reader.Close() }()
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			return err
		}
		commit, handleErr := c.Handle(msg)
		if !commit {
			if handleErr != nil {
				slog.Error("consume", "topic", msg.Topic, "partition", msg.Partition, "offset", msg.Offset, "error", handleErr)
			}
			reader.Close()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			// A later offset commit would skip this message. Rejoin from the last committed offset.
			reader = c.reader()
			continue
		}
		if err := reader.CommitMessages(ctx, msg); err != nil {
			return err
		}
	}
}

// ConsumeOne reads and handles a single message. The caller context bounds the wait.
func (c *Consumer) ConsumeOne(ctx context.Context) error {
	reader := c.reader()
	defer reader.Close()
	msg, err := reader.FetchMessage(ctx)
	if err != nil {
		return err
	}
	commit, err := c.Handle(msg)
	if err != nil {
		return err
	}
	if !commit {
		return fmt.Errorf("message at %s/%d/%d was not committed", msg.Topic, msg.Partition, msg.Offset)
	}
	return reader.CommitMessages(ctx, msg)
}

func (c *Consumer) reader() *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:        c.Brokers,
		GroupID:        c.GroupID,
		Topic:          c.Topic,
		MinBytes:       1,
		MaxBytes:       10e6,
		MaxWait:        500 * time.Millisecond,
		StartOffset:    kafka.FirstOffset,
		CommitInterval: 0,
	})
}

// Handle applies one message. commit is true only after the database change is durable
// or the message has been stored as a dead letter.
func (c *Consumer) Handle(msg kafka.Message) (commit bool, err error) {
	env, reason := envelope(msg)
	if reason != "" {
		return c.deadLetter(msg, env, reason)
	}
	switch env.EventType {
	case event.TypeCheckCompleted:
		var ev event.CheckCompleted
		if err := json.Unmarshal(msg.Value, &ev); err != nil {
			return c.deadLetter(msg, env, err.Error())
		}
		if ev.EventID == "" || ev.ServiceID == "" || ev.CheckID == 0 {
			return c.deadLetter(msg, env, "payload is missing event_id, service_id, or check_id")
		}
		if reason := matchMessage(msg, ev.EventID, ev.ServiceID); reason != "" {
			return c.deadLetter(msg, env, reason)
		}
		err = c.DB.ApplyCheckEvent(ev)
	default:
		return c.deadLetter(msg, env, fmt.Sprintf("unsupported event %s version %d", env.EventType, env.SchemaVersion))
	}
	if err == nil || errors.Is(err, db.ErrAlreadyProcessed) {
		return true, nil
	}
	return false, err
}

type messageEnvelope struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	SchemaVersion int    `json:"schema_version"`
	ServiceID     string `json:"service_id"`
}

func envelope(msg kafka.Message) (messageEnvelope, string) {
	var env messageEnvelope
	if len(msg.Value) == 0 || !json.Valid(msg.Value) {
		return env, "payload is not json"
	}
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		return env, err.Error()
	}
	if env.SchemaVersion != event.SchemaVersion {
		return env, fmt.Sprintf("unsupported event %s version %d", env.EventType, env.SchemaVersion)
	}
	if env.EventType != event.TypeCheckCompleted {
		return env, fmt.Sprintf("unsupported event %s version %d", env.EventType, env.SchemaVersion)
	}
	return env, ""
}

func matchMessage(msg kafka.Message, eventID, serviceID string) string {
	header := headerValue(msg, "event_id")
	if header != "" && header != eventID {
		return "event_id header does not match the payload"
	}
	if len(msg.Key) > 0 && string(msg.Key) != serviceID {
		return "message key does not match service_id"
	}
	return ""
}

func (c *Consumer) deadLetter(msg kafka.Message, env messageEnvelope, reason string) (bool, error) {
	payload := msg.Value
	if !json.Valid(payload) {
		wrapped, _ := json.Marshal(map[string]string{"raw": string(payload)})
		payload = wrapped
	}
	if err := c.DB.RecordDeadLetter(msg.Topic, msg.Partition, msg.Offset, env.EventID, env.EventType, payload, reason); err != nil {
		return false, err
	}
	return true, nil
}

func headerValue(msg kafka.Message, key string) string {
	for _, h := range msg.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
