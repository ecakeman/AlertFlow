package diagnostics

import (
	"encoding/json"
	"strings"
	"time"
)

const maxItems = 100

var (
	statuses = map[string]bool{"healthy": true, "degraded": true, "unhealthy": true, "unknown": true}
	kinds    = map[string]bool{
		"component_health": true, "dependency_health": true, "resource": true,
		"saturation":   true,
		"recent_error": true, "recent_change": true, "slow_operation": true,
	}
	// saturation is an internal level, same as resource: status is required.
	statusKinds = map[string]bool{
		"component_health": true, "dependency_health": true, "resource": true, "saturation": true,
	}
	summaryKinds = map[string]bool{"recent_error": true, "recent_change": true, "slow_operation": true}
)

// Item is one snapshot entry the collector can store.
type Item struct {
	Kind       string
	Name       string
	Status     string
	Summary    string
	ObservedAt time.Time
	Stale      bool
	EventID    string
	Attributes map[string]string
	Detail     json.RawMessage
}

// Snapshot is a validated diagnostics document.
type Snapshot struct {
	Coverage string
	Items    []Item
}

// ParseSnapshot applies the same acceptance rules as the agent's diagnostics
// client. eventTime is the fetch time used to mark entries older than five
// minutes as stale.
func ParseSnapshot(body []byte, eventTime time.Time) Snapshot {
	var doc struct {
		SchemaVersion string            `json:"schema_version"`
		Items         []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(body, &doc) != nil || (doc.SchemaVersion != "v1" && doc.SchemaVersion != "v2") {
		return Snapshot{Coverage: "invalid"}
	}
	if doc.Items == nil {
		return Snapshot{Coverage: "invalid"}
	}
	dropped := 0
	if len(doc.Items) > maxItems {
		dropped = len(doc.Items) - maxItems
		doc.Items = doc.Items[:maxItems]
	}
	var kept []Item
	stale := false
	for _, raw := range doc.Items {
		item, ok := parseItem(raw, eventTime)
		if !ok {
			dropped++
			continue
		}
		if item.Stale {
			stale = true
		}
		kept = append(kept, item)
	}
	coverage := "ok"
	switch {
	case len(kept) == 0 && dropped > 0:
		coverage = "invalid"
	case len(kept) == 0:
		coverage = "empty"
	case dropped > 0:
		coverage = "partial"
	case stale:
		coverage = "stale"
	}
	return Snapshot{Coverage: coverage, Items: kept}
}

func parseItem(raw []byte, eventTime time.Time) (Item, bool) {
	var in struct {
		Kind       string          `json:"kind"`
		Name       string          `json:"name"`
		Status     string          `json:"status"`
		Summary    string          `json:"summary"`
		ObservedAt string          `json:"observed_at"`
		EventID    string          `json:"event_id"`
		Attributes map[string]any  `json:"attributes"`
		Detail     json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &in) != nil || !kinds[in.Kind] || strings.TrimSpace(in.Name) == "" {
		return Item{}, false
	}
	observed, err := time.Parse(time.RFC3339, in.ObservedAt)
	if err != nil {
		return Item{}, false
	}
	if statusKinds[in.Kind] && !statuses[in.Status] {
		return Item{}, false
	}
	if summaryKinds[in.Kind] && strings.TrimSpace(in.Summary) == "" {
		return Item{}, false
	}
	if len(in.Attributes) > 20 {
		return Item{}, false
	}
	for _, value := range in.Attributes {
		if len(strings.TrimSpace(toString(value))) > 200 {
			return Item{}, false
		}
	}
	if len(in.Detail) > 0 && string(in.Detail) != "null" {
		if !json.Valid(in.Detail) || in.Detail[0] != '{' {
			return Item{}, false
		}
	}
	name := strings.TrimSpace(in.Name)
	if len(name) > 200 {
		name = name[:200]
	}
	attrs := map[string]string{}
	for key, value := range in.Attributes {
		attrs[key] = clipRunes(strings.TrimSpace(toString(value)), 200)
	}
	var detail json.RawMessage
	if len(in.Detail) > 0 && string(in.Detail) != "null" {
		var decoded any
		if json.Unmarshal(in.Detail, &decoded) != nil {
			return Item{}, false
		}
		normalized, err := json.Marshal(decoded)
		if err != nil {
			return Item{}, false
		}
		detail = normalized
	}
	item := Item{
		Kind: in.Kind, Name: name, Status: in.Status, Summary: clipRunes(in.Summary, 1024), ObservedAt: observed.UTC(),
		EventID: strings.TrimSpace(in.EventID), Attributes: attrs, Detail: detail,
	}
	if !eventTime.IsZero() && eventTime.Sub(observed) > 5*time.Minute {
		item.Stale = true
	}
	return item, true
}

func clipRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func toString(value any) string {
	text, ok := value.(string)
	if ok {
		return text
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}
