package api

import (
	"sync"
	"time"
)

// loginLimiter throttles attempts per key with a sliding window.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	max      int
}

const (
	loginWindow = time.Minute
	loginMax    = 10
	webhookMax  = 60
)

func newLoginLimiter() *loginLimiter {
	return newLimiter(loginMax)
}

func newWebhookLimiter() *loginLimiter {
	return newLimiter(webhookMax)
}

func newLimiter(max int) *loginLimiter {
	return &loginLimiter{attempts: make(map[string][]time.Time), max: max}
}

// allow records an attempt and reports whether it is within the rate limit.
func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-loginWindow)
	recent := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.max {
		l.attempts[key] = recent
		return false
	}
	l.attempts[key] = append(recent, time.Now())
	return true
}
