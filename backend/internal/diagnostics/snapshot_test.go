package diagnostics

import (
	"strings"
	"testing"
	"time"
)

func TestParseItemClipsSummary(t *testing.T) {
	raw := []byte(`{"kind":"recent_error","name":"db","summary":"` + strings.Repeat("长", 1100) + `","observed_at":"2026-10-28T00:00:00Z"}`)
	item, ok := parseItem(raw, time.Now())
	if !ok {
		t.Fatal("item was rejected")
	}
	if n := len([]rune(item.Summary)); n != 1024 {
		t.Fatalf("summary runes = %d", n)
	}
}
