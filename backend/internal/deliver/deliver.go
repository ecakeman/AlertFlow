// Package deliver sends one notification_deliveries row at a time.
// It does not call Dispatcher.Notify, which would fan out to every channel.
// A crash after the remote service accepts the request and before PostgreSQL
// records success can send the same task again. That is at-least-once delivery.
package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/notify"
)

const (
	// MaxAttempts is how many sends a task gets before it becomes dead.
	MaxAttempts = 5
	// LeaseSeconds matches notify.SendTimeout. There is no lease renewal.
	LeaseSeconds = 15
)

// Worker claims due delivery tasks and sends each one outside a database transaction.
type Worker struct {
	DB    *db.DB
	Owner string
	// Now supplies unix seconds. Tests set it so backoff does not sleep.
	Now func() int64
}

// New builds a worker. An empty owner is unique to this process.
func New(database *db.DB, owner string) *Worker {
	if owner == "" {
		owner = fmt.Sprintf("delivery-%d", os.Getpid())
	}
	return &Worker{DB: database, Owner: owner}
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
		rows, err := w.DB.ClaimDeliveries(w.Owner, now, now+LeaseSeconds, 1)
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
	if err := w.DB.FailDelivery(row.ID, w.Owner, now, status, cause.Error(), next); err != nil {
		return err
	}
	return cause
}

func messageFrom(raw json.RawMessage) (notify.Message, error) {
	var snap db.DeliverySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return notify.Message{}, err
	}
	switch snap.Event {
	case string(notify.EventIncidentStart), string(notify.EventIncidentResolve), string(notify.EventWarning):
	default:
		return notify.Message{}, fmt.Errorf("unsupported delivery event %q", snap.Event)
	}
	msg := notify.Message{
		Event:       notify.Event(snap.Event),
		ServiceID:   snap.ServiceID,
		ServiceName: snap.ServiceName,
		ServiceURL:  snap.ServiceURL,
		StartedAt:   time.Unix(snap.StartedAt, 0),
		Attempts:    snap.Attempts,
		Error:       snap.Error,
	}
	if snap.IncidentID != nil {
		msg.IncidentID = *snap.IncidentID
	}
	if snap.ResolvedAt != nil {
		resolved := time.Unix(*snap.ResolvedAt, 0)
		msg.ResolvedAt = &resolved
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
