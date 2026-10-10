package notify

import "testing"

func TestStatusErrorRetryableCodes(t *testing.T) {
	permanent := []int{400, 401, 403, 404, 422}
	retryable := []int{408, 425, 429, 500, 503}
	for _, code := range permanent {
		if !(&StatusError{Code: code}).Permanent() {
			t.Errorf("status %d should be permanent", code)
		}
	}
	for _, code := range retryable {
		if (&StatusError{Code: code}).Permanent() {
			t.Errorf("status %d should be retryable", code)
		}
	}
}
