package event

import "testing"

func TestDecide(t *testing.T) {
	tests := []struct {
		prev, cur string
		want      Transition
	}{
		{"online", "online", TransitionNone},
		{"online", "offline", TransitionOpen},
		{"online", "warning", TransitionWarning},
		{"warning", "online", TransitionNone},
		{"warning", "offline", TransitionOpen},
		{"offline", "online", TransitionResolve},
		{"offline", "warning", TransitionResolve},
		{"offline", "offline", TransitionNone},
		{"unknown", "offline", TransitionOpen},
		{"unknown", "warning", TransitionWarning},
		{"unknown", "online", TransitionNone},
	}
	for _, tt := range tests {
		if got := Decide(tt.prev, tt.cur); got != tt.want {
			t.Errorf("Decide(%s, %s) = %q, want %q", tt.prev, tt.cur, got, tt.want)
		}
	}
}
