package llmgateway

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Quota enforces a per-tenant daily token budget. Reserve is atomic so bursts
// cannot overshoot the limit (the source of upstream 429s).
type Quota struct {
	limit int64
	store tokenStore
}

type tokenStore interface {
	Reserve(ctx context.Context, key string, amount, limit int64) (int64, bool, error)
	Add(ctx context.Context, key string, delta int64) error
}

func NewQuota(driver, addr string, limit int64) *Quota {
	if driver == "redis" && addr != "" {
		client := redis.NewClient(&redis.Options{Addr: addr})
		return &Quota{limit: limit, store: &redisTokenStore{client: client}}
	}
	return &Quota{limit: limit, store: &memoryTokenStore{values: map[string]int64{}}}
}

// Reserve adds the estimate and reports whether the tenant is within budget.
func (q *Quota) Reserve(key string, amount int64) (bool, error) {
	if q == nil {
		return true, nil
	}
	if amount < 0 {
		amount = 0
	}
	_, allowed, err := q.store.Reserve(context.Background(), key, amount, q.limit)
	return allowed, err
}

// Adjust corrects the reservation to the actual usage (may be negative).
func (q *Quota) Adjust(key string, delta int64) error {
	if q == nil || delta == 0 {
		return nil
	}
	return q.store.Add(context.Background(), key, delta)
}

// DailyKey buckets usage per tenant and UTC day.
func DailyKey(tenant string, now time.Time) string {
	return fmt.Sprintf("nexusflow:llm:tokens:%s:%s", tenant, now.UTC().Format("20060102"))
}

type memoryTokenStore struct {
	mu     sync.Mutex
	values map[string]int64
}

func (s *memoryTokenStore) Reserve(_ context.Context, key string, amount, limit int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.values[key] + amount
	if limit > 0 && next > limit {
		return s.values[key], false, nil
	}
	s.values[key] = next
	return next, true, nil
}

func (s *memoryTokenStore) Add(_ context.Context, key string, delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] += delta
	if s.values[key] < 0 {
		s.values[key] = 0
	}
	return nil
}

type redisTokenStore struct {
	client *redis.Client
}

func (s *redisTokenStore) Reserve(ctx context.Context, key string, amount, limit int64) (int64, bool, error) {
	next, err := s.client.IncrBy(ctx, key, amount).Result()
	if err != nil {
		return 0, false, err
	}
	if err := s.client.Expire(ctx, key, 48*time.Hour).Err(); err != nil {
		return next, false, err
	}
	if limit > 0 && next > limit {
		// Roll back the reservation so a rejected burst does not inflate usage.
		if err := s.client.DecrBy(ctx, key, amount).Err(); err != nil {
			return next, false, err
		}
		return next - amount, false, nil
	}
	return next, true, nil
}

func (s *redisTokenStore) Add(ctx context.Context, key string, delta int64) error {
	if delta == 0 {
		return nil
	}
	_, err := s.client.IncrBy(ctx, key, delta).Result()
	return err
}

// ParseLimit accepts an integer token budget from the environment.
func ParseLimit(raw string) int64 {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}
