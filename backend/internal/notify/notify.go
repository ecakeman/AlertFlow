// Package notify delivers incident notifications to the diagnostic agent.
// Telegram, Slack, email and generic webhook senders are retired. A Dispatcher
// only sends a test through one registered sender.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/ecakeman/alertflow/internal/agentmessage"
	"github.com/ecakeman/alertflow/internal/db"
)

// ErrNoSender is returned when no sender is registered for an integration type.
var ErrNoSender = errors.New("no sender registered for integration type")

// Event is the kind of incident transition being notified.
type Event string

const (
	EventIncidentStart   Event = "incident_start"
	EventIncidentResolve Event = "incident_resolve"
	// EventWarning is a check that failed and then recovered on a retry. It has
	// no incident, and only integrations that opted in receive it.
	EventWarning Event = "warning"
	// EventChatRequested is a troubleshooting question for the diagnostic agent.
	// Only the agent channel can handle it; chat requests are queued solely for
	// that integration, so a notification channel never sees one.
	EventChatRequested Event = db.EventChatRequested
)

// ChatRequest is a read-only question queued for the diagnostic agent. It rides
// the notification delivery path so it inherits the same leases, retries and
// dead-lettering rather than being a second delivery mechanism.
type ChatRequest struct {
	RequestID string
	Message   string
	History   json.RawMessage
}

// Message is the payload handed to each Sender.
type Message struct {
	Event       Event
	IncidentID  int64 // 0 for a warning: no incident is opened
	ServiceID   string
	ServiceName string
	ServiceURL  string
	OccurredAt  time.Time  // this notification's event time
	StartedAt   time.Time  // incident start; zero for a warning
	ResolvedAt  *time.Time // set for resolve events
	Attempts    int        // warning: how many attempts the cycle needed
	Error       string     // warning: why the first attempt failed
	CheckID     int64      // 0 when the event is not a check
	DeliveryID  int64      // notification_deliveries.id, used when IncidentID is 0
	// Chat is set for a chat_requested delivery and nil for every notification.
	Chat *ChatRequest
}

// Down reports whether the message is a service-down (start) event.
func (m Message) Down() bool { return m.Event == EventIncidentStart }

// Warning reports whether the message is a recovered-on-retry event.
func (m Message) Warning() bool { return m.Event == EventWarning }

// Subject is a short one-line summary suitable for an email subject / message title.
func (m Message) Subject() string {
	switch {
	case m.Down():
		return "🔴 " + m.ServiceName + " is DOWN"
	case m.Warning():
		return "🟠 " + m.ServiceName + " recovered after a retry"
	default:
		return "🟢 " + m.ServiceName + " has RECOVERED"
	}
}

// Body is a plain-text description of the event.
func (m Message) Body() string {
	if m.Down() {
		return m.ServiceName + " (" + m.ServiceURL + ") went down at " +
			m.StartedAt.UTC().Format(time.RFC1123) + "."
	}
	if m.Warning() {
		// The first attempt's error is the only record of why it blipped, so it
		// belongs in the message even though the service is up.
		b := m.ServiceName + " (" + m.ServiceURL + ") failed a check at " +
			m.eventTime().UTC().Format(time.RFC1123) + " but recovered on attempt " +
			strconv.Itoa(m.Attempts) + "."
		if m.Error != "" {
			b += " First failure: " + m.Error + "."
		}
		return b
	}
	when := m.StartedAt
	if m.ResolvedAt != nil {
		when = *m.ResolvedAt
	}
	return m.ServiceName + " (" + m.ServiceURL + ") recovered at " +
		when.UTC().Format(time.RFC1123) + "."
}

func (m Message) eventTime() time.Time {
	if !m.OccurredAt.IsZero() {
		return m.OccurredAt
	}
	return m.StartedAt
}

// severityOf 复用共享映射，避免 HTTP 与 gRPC 两条路径对 severity 的判断分叉。
func severityOf(event Event) string {
	return agentmessage.Severity(string(event))
}

// Sender delivers a message to one channel. config is the integration's stored
// JSON blob (shape is sender-specific).
type Sender interface {
	Send(ctx context.Context, config json.RawMessage, msg Message) error
}

// senders is the registry of channel implementations, populated by each
// sender file's init(). Empty until the concrete senders are registered.
var senders = map[string]Sender{}

func register(kind string, s Sender) { senders[kind] = s }

// Register installs a sender for a channel type. Senders that need runtime
// dependencies (the Agent webhook needs the query layer) cannot be built by
// init(), so the process that owns those dependencies registers them at
// startup instead.
func Register(kind string, s Sender) { register(kind, s) }

// SenderFor returns the registered sender for a channel type, if any.
func SenderFor(kind string) (Sender, bool) {
	s, ok := senders[kind]
	return s, ok
}
