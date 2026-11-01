package cache

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestAsideNilAndUnavailable(t *testing.T) {
	var got int
	err := (Aside{}).Get(context.Background(), "k", &got, func() (any, error) { return 7, nil })
	if err != nil || got != 7 {
		t.Fatalf("nil redis got %d %v", got, err)
	}
	aside := Aside{Client: redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, ReadTimeout: 50 * time.Millisecond,
	}), TTL: time.Second}
	got = 0
	err = aside.Get(context.Background(), "k", &got, func() (any, error) { return 9, nil })
	if err != nil || got != 9 {
		t.Fatalf("down redis got %d %v", got, err)
	}
}
