// Package deliver sends one notification_deliveries row at a time.
// The only sender is the diagnostic agent webhook.
// A crash after the remote service accepts the request and before PostgreSQL
// records success can send the same task again. That is at-least-once delivery.
package deliver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/notify"
)

const (
	// MaxAttempts is how many sends a task gets before it becomes dead.
	MaxAttempts = 5
	// LeaseSeconds is longer than one send so the worker can still record the
	// outcome after SendTimeout. There is no lease renewal.
	LeaseSeconds = 45
)

// TaskClient accepts one internal agent task over gRPC. Duplicate is success.
type TaskClient interface {
	Accept(ctx context.Context, row db.ClaimedDelivery) error
}

// Worker claims due delivery tasks and sends each one outside a database transaction.
type Worker struct {
	DB    *db.DB
	Owner string
	Tasks TaskClient
	// Now supplies unix seconds. Tests set it so backoff does not sleep.
	Now func() int64
}

// New builds a worker. An empty owner is generated once and kept for the
// process lifetime. Callers can pass an owner to make tests deterministic.
func New(database *db.DB, owner string) *Worker {
	if owner == "" {
		owner = newOwner()
	}
	return &Worker{DB: database, Owner: owner}
}

func newOwner() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	return "delivery-" + hex.EncodeToString(buf[:])
}

func (w *Worker) now() int64 {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().Unix()
}

// Run sends until ctx is cancelled. A lost lease or a send error does not stop the process.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := w.ProcessDue(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("deliver", "error", err)
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

// ProcessDue sends every task that is due. The clock comes from Worker.Now.
func (w *Worker) ProcessDue(ctx context.Context) (int, error) {
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		now := w.now()
		rows, err := w.DB.ClaimDeliveries(w.Owner, now, now+LeaseSeconds, 1, MaxAttempts)
		if err != nil {
			return n, err
		}
		if len(rows) == 0 {
			return n, nil
		}
		if err := w.sendOne(ctx, rows[0]); err != nil && !errors.Is(err, db.ErrLeaseLost) {
			slog.Warn("deliver", "delivery", rows[0].ID, "error", err)
		}
		n++
	}
}

func (w *Worker) sendOne(ctx context.Context, row db.ClaimedDelivery) error {
	msg, err := messageFrom(row.Payload)
	if err != nil {
		return w.fail(row, err, true)
	}
	msg.DeliveryID = row.ID
	if row.IntegrationID == 0 {
		if w.Tasks == nil {
			return w.fail(row, errors.New("agent grpc target is not configured"), false)
		}
		sendCtx, cancel := context.WithTimeout(ctx, notify.SendTimeout)
		defer cancel()
		if err := w.Tasks.Accept(sendCtx, row); err != nil {
			return w.fail(row, err, permanent(err))
		}
		return w.DB.SucceedDelivery(row.ID, w.Owner, w.now(), w.now())
	}
	sender, ok := notify.SenderFor(row.IntegrationType)
	if !ok {
		return w.fail(row, notify.ErrNoSender, true)
	}
	sendCtx, cancel := context.WithTimeout(ctx, notify.SendTimeout)
	defer cancel()
	err = sender.Send(sendCtx, row.Config, msg)
	if err != nil {
		return w.fail(row, err, permanent(err))
	}
	finished := w.now()
	if err := w.DB.SucceedDelivery(row.ID, w.Owner, finished, finished); err != nil {
		return err
	}
	return nil
}

func (w *Worker) fail(row db.ClaimedDelivery, cause error, perm bool) error {
	now := w.now()
	status := "retry_wait"
	next := now + backoff(row.AttemptCount)
	if perm || row.AttemptCount >= MaxAttempts {
		status = "dead"
		next = now
	}
	safe := redactError(cause, row.Config)
	if err := w.DB.FailDelivery(row.ID, w.Owner, now, status, safe, next); err != nil {
		return err
	}
	return errors.New(safe)
}

func messageFrom(raw json.RawMessage) (notify.Message, error) {
	var snap db.DeliverySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return notify.Message{}, err
	}
	switch snap.Event {
	case string(notify.EventIncidentStart), string(notify.EventIncidentResolve),
		string(notify.EventWarning), string(notify.EventChatRequested):
	default:
		return notify.Message{}, fmt.Errorf("unsupported delivery event %q", snap.Event)
	}
	occurredAt := snap.OccurredAt
	if occurredAt == 0 {
		occurredAt = snap.StartedAt
	}
	msg := notify.Message{
		Event:       notify.Event(snap.Event),
		ServiceID:   snap.ServiceID,
		ServiceName: snap.ServiceName,
		ServiceURL:  snap.ServiceURL,
		OccurredAt:  time.Unix(occurredAt, 0),
		StartedAt:   time.Unix(snap.StartedAt, 0),
		Attempts:    snap.Attempts,
		Error:       snap.Error,
		CheckID:     snap.CheckID,
	}
	if snap.IncidentID != nil {
		msg.IncidentID = *snap.IncidentID
	}
	if snap.ResolvedAt != nil {
		resolved := time.Unix(*snap.ResolvedAt, 0)
		msg.ResolvedAt = &resolved
	}
	if snap.Chat != nil {
		msg.Chat = &notify.ChatRequest{
			RequestID: snap.Chat.RequestID,
			Message:   snap.Chat.Message,
			History:   snap.Chat.History,
		}
	}
	return msg, nil
}

func permanent(err error) bool {
	var status *notify.StatusError
	if errors.As(err, &status) && status.Permanent() {
		return true
	}
	if errors.Is(err, notify.ErrNoSender) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "is required") || strings.Contains(msg, "unsupported delivery event")
}

func redactError(cause error, config json.RawMessage) string {
	text := cause.Error()
	secrets := secretValues(config)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if len(secret) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return text
}

func secretValues(config json.RawMessage) []string {
	var root any
	if err := json.Unmarshal(config, &root); err != nil {
		return nil
	}
	var out []string
	collectSecrets(root, "", false, &out)
	return out
}

func collectSecrets(value any, key string, inHeaders bool, out *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			collectSecrets(child, childKey, inHeaders || strings.EqualFold(childKey, "headers"), out)
		}
	case []any:
		for _, child := range typed {
			collectSecrets(child, key, inHeaders, out)
		}
	case string:
		if inHeaders || secretKey(key) {
			*out = append(*out, typed)
			*out = append(*out, urlSecretParts(typed)...)
		}
	}
}

func secretKey(key string) bool {
	switch strings.ToLower(key) {
	case "url", "webhookurl", "bottoken", "password", "token", "secret", "authorization", "apikey":
		return true
	default:
		return false
	}
}

func urlSecretParts(raw string) []string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil
	}
	parts := strings.Split(parsed.EscapedPath(), "/")
	if parsed.User != nil {
		if name := parsed.User.Username(); name != "" {
			parts = append(parts, name)
		}
		if password, ok := parsed.User.Password(); ok {
			parts = append(parts, password)
		}
	}
	for key, values := range parsed.Query() {
		parts = append(parts, key)
		parts = append(parts, values...)
	}
	return parts
}

func backoff(attempt int) int64 {
	delay := int64(5)
	for i := 1; i < attempt && delay < 300; i++ {
		delay *= 2
	}
	if delay > 300 {
		delay = 300
	}
	return delay
}
