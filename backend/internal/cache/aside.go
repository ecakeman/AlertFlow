package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// Aside 只缓存读模型。Redis 不可用时回退到 loader。
type Aside struct {
	Client *redis.Client
	TTL    time.Duration
}

func (a Aside) Get(ctx context.Context, key string, dst any, loader func() (any, error)) error {
	if a.Client != nil {
		raw, err := a.Client.Get(ctx, key).Bytes()
		if err == nil {
			return json.Unmarshal(raw, dst)
		}
	}
	value, err := loader()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, dst); err != nil {
		return err
	}
	if a.Client != nil {
		ttl := a.TTL
		if ttl <= 0 {
			ttl = 5 * time.Second
		}
		_ = a.Client.Set(ctx, key, encoded, ttl).Err()
	}
	return nil
}

func (a Aside) Delete(ctx context.Context, keys ...string) {
	if a.Client == nil || len(keys) == 0 {
		return
	}
	_ = a.Client.Del(ctx, keys...).Err()
}
