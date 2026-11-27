package identity

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func (r *RedisStore) knowledgeBaseKey(tenantID, id string) string {
	return "identity:kb:" + tenantID + ":" + id
}

func (r *RedisStore) knowledgeBaseNameKey(tenantID, name string) string {
	return "identity:kbname:" + tenantID + ":" + strings.ToLower(strings.TrimSpace(name))
}

func (r *RedisStore) CreateKnowledgeBase(ctx context.Context, kb KnowledgeBase) (KnowledgeBase, error) {
	kb.TenantID = tenantOr(kb.TenantID)
	kb.Name = strings.TrimSpace(kb.Name)
	if kb.Name == "" {
		return KnowledgeBase{}, ErrKnowledgeBaseName
	}
	if kb.ID == "" {
		kb.ID = uuid.NewString()
	}
	now := time.Now()
	if kb.CreatedAt.IsZero() {
		kb.CreatedAt = now
	}
	kb.UpdatedAt = now
	payload, err := json.Marshal(kb)
	if err != nil {
		return KnowledgeBase{}, err
	}
	nameKey := r.knowledgeBaseNameKey(kb.TenantID, kb.Name)
	ok, err := r.rdb.SetNX(ctx, nameKey, kb.ID, 0).Result()
	if err != nil {
		return KnowledgeBase{}, err
	}
	if !ok {
		return KnowledgeBase{}, ErrKnowledgeBaseExists
	}
	idKey := r.knowledgeBaseKey(kb.TenantID, kb.ID)
	ok, err = r.rdb.SetNX(ctx, idKey, payload, 0).Result()
	if err != nil || !ok {
		_ = r.rdb.Del(ctx, nameKey).Err()
		if err != nil {
			return KnowledgeBase{}, err
		}
		return KnowledgeBase{}, ErrKnowledgeBaseExists
	}
	return kb, nil
}

func (r *RedisStore) GetKnowledgeBase(ctx context.Context, tenantID, id string) (KnowledgeBase, bool, error) {
	raw, err := r.rdb.Get(ctx, r.knowledgeBaseKey(tenantOr(tenantID), id)).Bytes()
	if err == redis.Nil {
		return KnowledgeBase{}, false, nil
	}
	if err != nil {
		return KnowledgeBase{}, false, err
	}
	var kb KnowledgeBase
	if err := json.Unmarshal(raw, &kb); err != nil {
		return KnowledgeBase{}, false, err
	}
	return kb, true, nil
}

func (r *RedisStore) ListKnowledgeBases(ctx context.Context, tenantID string) ([]KnowledgeBase, error) {
	tenantID = tenantOr(tenantID)
	keys, err := r.scan(ctx, "identity:kb:"+tenantID+":*")
	if err != nil {
		return nil, err
	}
	out := make([]KnowledgeBase, 0, len(keys))
	for _, key := range keys {
		raw, err := r.rdb.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var kb KnowledgeBase
		if json.Unmarshal(raw, &kb) == nil {
			out = append(out, kb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (r *RedisStore) UpdateKnowledgeBase(ctx context.Context, patch KnowledgeBase) (KnowledgeBase, error) {
	tenantID := tenantOr(patch.TenantID)
	existing, ok, err := r.GetKnowledgeBase(ctx, tenantID, patch.ID)
	if err != nil {
		return KnowledgeBase{}, err
	}
	if !ok {
		return KnowledgeBase{}, ErrKnowledgeBaseNotFound
	}
	oldNameKey := r.knowledgeBaseNameKey(tenantID, existing.Name)
	if name := strings.TrimSpace(patch.Name); name != "" && !strings.EqualFold(name, existing.Name) {
		newNameKey := r.knowledgeBaseNameKey(tenantID, name)
		ok, err := r.rdb.SetNX(ctx, newNameKey, existing.ID, 0).Result()
		if err != nil {
			return KnowledgeBase{}, err
		}
		if !ok {
			return KnowledgeBase{}, ErrKnowledgeBaseExists
		}
		existing.Name = name
		_ = r.rdb.Del(ctx, oldNameKey).Err()
	} else if name != "" {
		existing.Name = name
	}
	existing.Description = patch.Description
	existing.UpdatedAt = time.Now()
	payload, err := json.Marshal(existing)
	if err != nil {
		return KnowledgeBase{}, err
	}
	if err := r.rdb.Set(ctx, r.knowledgeBaseKey(tenantID, existing.ID), payload, 0).Err(); err != nil {
		return KnowledgeBase{}, err
	}
	return existing, nil
}

func (r *RedisStore) DeleteKnowledgeBase(ctx context.Context, tenantID, id string) error {
	if id == DefaultKnowledgeBaseID {
		return ErrDefaultKnowledgeBase
	}
	tenantID = tenantOr(tenantID)
	kb, ok, err := r.GetKnowledgeBase(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrKnowledgeBaseNotFound
	}
	pipe := r.rdb.TxPipeline()
	pipe.Del(ctx, r.knowledgeBaseKey(tenantID, id))
	pipe.Del(ctx, r.knowledgeBaseNameKey(tenantID, kb.Name))
	_, err = pipe.Exec(ctx)
	return err
}
