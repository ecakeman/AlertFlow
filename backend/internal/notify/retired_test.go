package notify

import "testing"

func TestRetiredChannelsHaveNoSender(t *testing.T) {
	for _, kind := range []string{"telegram", "slack", "email", "webhook"} {
		if _, ok := SenderFor(kind); ok {
			t.Fatalf("%s still has a sender", kind)
		}
	}
}
