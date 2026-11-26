package revocation

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisKeyPrefix = "auth:revoked:"

// RedisBlacklist shares revocations across processes. Each revoked token id is
// a key with a TTL equal to the token's remaining lifetime, so Redis expires
// entries automatically and the list never needs manual pruning.
type RedisBlacklist struct {
	rdb *redis.Client
}

func NewRedisBlacklist(addr string) (*RedisBlacklist, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &RedisBlacklist{rdb: rdb}, nil
}

func (r *RedisBlacklist) Revoke(ctx context.Context, tokenID string, ttl time.Duration) error {
	if tokenID == "" || ttl <= 0 {
		return nil
	}
	return r.rdb.Set(ctx, redisKeyPrefix+tokenID, "1", ttl).Err()
}

func (r *RedisBlacklist) Revoked(ctx context.Context, tokenID string) (bool, error) {
	if tokenID == "" {
		return false, nil
	}
	n, err := r.rdb.Exists(ctx, redisKeyPrefix+tokenID).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *RedisBlacklist) Close() error { return r.rdb.Close() }
