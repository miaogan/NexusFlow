package state

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrConflict = errors.New("concurrent state update")

// RedisStore shares task state across ingestion, processing and gateway
// processes. The idempotency key prevents duplicate work on redelivery.
type RedisStore struct {
	rdb *redis.Client
}

func NewRedisStore(addr string) (*RedisStore, error) {
	if addr == "" {
		return nil, errors.New("redis address is empty")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &RedisStore{rdb: rdb}, nil
}

func (s *RedisStore) Close() error { return s.rdb.Close() }

func taskKey(id string) string { return "nexusflow:task:" + id }

func idemKey(tenantID, hash string) string {
	return "nexusflow:idem:" + tenantID + ":" + hash
}

func (s *RedisStore) Create(ctx context.Context, t Task) (Task, bool, error) {
	if t.ID == "" {
		return Task{}, false, errors.New("task id is required")
	}
	exists, err := s.rdb.Exists(ctx, taskKey(t.ID)).Result()
	if err != nil {
		return Task{}, false, err
	}
	if exists > 0 {
		return Task{}, false, ErrDuplicateTask
	}

	if t.ContentHash != "" {
		key := idemKey(t.TenantID, t.ContentHash)
		ok, err := s.rdb.SetNX(ctx, key, t.ID, 0).Result()
		if err != nil {
			return Task{}, false, err
		}
		if !ok {
			id, err := s.rdb.Get(ctx, key).Result()
			if err == nil {
				if existing, found, err := s.Get(ctx, id); err == nil && found {
					return existing, false, nil
				}
			}
			_ = s.rdb.Set(ctx, key, t.ID, 0)
		}
	}

	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if t.State == "" {
		t.State = Pending
	}
	t.Progress = progress[t.State]

	meta, _ := json.Marshal(t.Metadata)
	fields := map[string]any{
		"id":           t.ID,
		"tenant_id":    t.TenantID,
		"state":        string(t.State),
		"progress":     t.Progress,
		"error_msg":    t.ErrorMsg,
		"content_hash": t.ContentHash,
		"metadata":     string(meta),
		"created_at":   t.CreatedAt.Format(time.RFC3339Nano),
		"updated_at":   t.UpdatedAt.Format(time.RFC3339Nano),
	}
	if err := s.rdb.HSet(ctx, taskKey(t.ID), fields).Err(); err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

func (s *RedisStore) Get(ctx context.Context, id string) (Task, bool, error) {
	vals, err := s.rdb.HGetAll(ctx, taskKey(id)).Result()
	if err != nil {
		return Task{}, false, err
	}
	if len(vals) == 0 {
		return Task{}, false, nil
	}

	t := Task{
		ID:          vals["id"],
		TenantID:    vals["tenant_id"],
		State:       State(vals["state"]),
		ErrorMsg:    vals["error_msg"],
		ContentHash: vals["content_hash"],
	}
	progressVal, _ := strconv.ParseInt(vals["progress"], 10, 32)
	t.Progress = int32(progressVal)
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, vals["created_at"])
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, vals["updated_at"])
	if raw := vals["metadata"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &t.Metadata)
	}
	return t, true, nil
}

func (s *RedisStore) Transition(ctx context.Context, id string, to State, errMsg string) (Task, error) {
	key := taskKey(id)
	for attempt := 0; attempt < 5; attempt++ {
		err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
			vals, err := tx.HGetAll(ctx, key).Result()
			if err != nil {
				return err
			}
			if len(vals) == 0 {
				return ErrTaskNotFound
			}
			from := State(vals["state"])
			if !CanTransition(from, to) {
				return ErrInvalidState
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.HSet(ctx, key,
					"state", string(to),
					"progress", progress[to],
					"error_msg", errMsg,
					"updated_at", time.Now().Format(time.RFC3339Nano),
				)
				return nil
			})
			return err
		}, key)

		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if err != nil {
			return Task{}, err
		}
		t, _, err := s.Get(ctx, id)
		return t, err
	}
	return Task{}, ErrConflict
}
