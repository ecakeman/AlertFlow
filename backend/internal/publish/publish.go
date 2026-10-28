// Package publish sends outbox rows to Kafka. It does not open incidents.
// One process should run it: a second publisher can republish after a lease
// expires, and the consumer has to ignore the duplicate.
package publish

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/event"
)

const (
	// Topic is the check-event topic. Keys are service ids, so one service
	// stays in one partition.
	Topic = "alert-events"
	// Partitions is fixed and small. More consumers than this do not add parallelism.
	Partitions   = 3
	leaseSeconds = 30
)

type messageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Publisher claims outbox rows, waits for a broker ack, then marks them published.
type Publisher struct {
	DB      *db.DB
	Brokers []string
	Topic   string
	Owner   string
	writer  messageWriter
}

// New builds a publisher. brokers is a comma-separated list from ALERTFLOW_KAFKA_BROKERS.
func New(database *db.DB, brokers, topic, owner string) (*Publisher, error) {
	list := splitBrokers(brokers)
	if len(list) == 0 {
		return nil, fmt.Errorf("kafka brokers are empty (set ALERTFLOW_KAFKA_BROKERS)")
	}
	if topic == "" {
		topic = Topic
	}
	if owner == "" {
		owner = "publisher"
	}
	return &Publisher{DB: database, Brokers: list, Topic: topic, Owner: owner}, nil
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

// Run publishes until ctx is cancelled. Kafka errors are recorded on the row
// and retried later; they do not stop the process.
func (p *Publisher) Run(ctx context.Context) error {
	if err := EnsureTopic(ctx, p.Brokers, p.Topic, Partitions); err != nil {
		return err
	}
	p.writer = p.newWriter()
	defer p.writer.Close()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := p.publishAvailable(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("publish", "error", err)
		}
		wait := time.Second
		if n > 0 && err == nil {
			wait = 50 * time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// PublishOnce sends every row that is due now, in outbox id order, one at a time.
func (p *Publisher) PublishOnce(ctx context.Context) (int, error) {
	if err := EnsureTopic(ctx, p.Brokers, p.Topic, Partitions); err != nil {
		return 0, err
	}
	if p.writer == nil {
		p.writer = p.newWriter()
	}
	return p.publishAvailable(ctx)
}

// Close releases the Kafka writer.
func (p *Publisher) Close() error {
	if p.writer == nil {
		return nil
	}
	return p.writer.Close()
}

func (p *Publisher) newWriter() *kafka.Writer {
	return &kafka.Writer{
		Addr:                   kafka.TCP(p.Brokers...),
		Topic:                  p.Topic,
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireOne,
		AllowAutoTopicCreation: true,
		BatchTimeout:           time.Millisecond,
		WriteTimeout:           5 * time.Second,
		ReadTimeout:            5 * time.Second,
		Transport:              &kafka.Transport{DialTimeout: 2 * time.Second},
	}
}

func (p *Publisher) publishAvailable(ctx context.Context) (int, error) {
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		now := time.Now().Unix()
		rows, err := p.DB.ClaimOutbox(p.Owner, now, now+leaseSeconds, 1)
		if err != nil {
			return n, err
		}
		if len(rows) == 0 {
			return n, nil
		}
		if err := p.publishOne(ctx, rows[0]); err != nil {
			return n, err
		}
		n++
	}
}

func (p *Publisher) publishOne(ctx context.Context, row db.OutboxEvent) error {
	var body struct {
		EventID       string `json:"event_id"`
		EventType     string `json:"event_type"`
		SchemaVersion int    `json:"schema_version"`
		ServiceID     string `json:"service_id"`
		OccurredAt    int64  `json:"occurred_at"`
		Epoch         *int64 `json:"epoch"`
	}
	if err := json.Unmarshal(row.Payload, &body); err != nil {
		return p.fail(row, err)
	}
	if body.EventID != row.EventID || body.SchemaVersion != event.SchemaVersion || body.EventType != row.EventType {
		return p.fail(row, errors.New("payload does not match the outbox row"))
	}
	if body.EventType != event.TypeCheckCompleted {
		return p.fail(row, fmt.Errorf("unsupported event %s", body.EventType))
	}
	retired, err := p.DB.ServiceRetired(body.ServiceID, body.OccurredAt, body.Epoch)
	if err != nil {
		return p.fail(row, err)
	}
	if retired {
		now := time.Now().Unix()
		if err := p.DB.ForgetLeasedOutbox(row.ID, p.Owner, now); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		slog.Info("publisher skipped a retired service", "service", body.ServiceID, "event", row.EventID)
		return nil
	}
	key := row.MessageKey
	if key == "" {
		key = body.ServiceID
	}
	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(key),
		Value: row.Payload,
		Headers: []kafka.Header{{
			Key:   "event_id",
			Value: []byte(row.EventID),
		}},
	})
	if err != nil {
		return p.fail(row, err)
	}
	publishedAt := time.Now().Unix()
	if err := p.DB.MarkOutboxPublished(row.ID, p.Owner, publishedAt, publishedAt); err != nil {
		return fmt.Errorf("broker acked event %s but the lease could not record it: %w", row.EventID, err)
	}
	return nil
}

func (p *Publisher) fail(row db.OutboxEvent, cause error) error {
	delay := int64(5)
	for i := 0; i < row.AttemptCount && delay < 300; i++ {
		delay *= 2
	}
	if delay > 300 {
		delay = 300
	}
	now := time.Now().Unix()
	next := now + delay
	if recErr := p.DB.RecordOutboxFailure(row.ID, p.Owner, now, cause.Error(), next); recErr != nil {
		return fmt.Errorf("publish %s: %w (record failure: %v)", row.EventID, cause, recErr)
	}
	return fmt.Errorf("publish %s: %w", row.EventID, cause)
}
