package notify

import (
	"context"

	"github.com/ecakeman/alertflow/internal/db"
)

// Dispatcher sends one test notification through a single integration.
type Dispatcher struct {
	db *db.DB
}

// NewDispatcher builds a dispatcher backed by database.
func NewDispatcher(database *db.DB) *Dispatcher {
	return &Dispatcher{db: database}
}

// Test delivers msg through one integration synchronously, returning the send
// error (used by the "send test notification" endpoint). It does not log.
func (d *Dispatcher) Test(ctx context.Context, in db.Integration, msg Message) error {
	sender, ok := SenderFor(in.Type)
	if !ok {
		return ErrNoSender
	}
	return sender.Send(ctx, in.Config, msg)
}
