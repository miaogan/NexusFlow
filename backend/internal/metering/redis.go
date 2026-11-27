package metering

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisStore shares metering counters between gateway replicas. Storage lives
// in per-knowledge-base hash fields so an over-subtract in one namespace can
// never drain another; tenant totals are summed on read.
//
//	nexusflow:metering:<tenant>  queries
//	                             kb:<kb_id>:storage / kb:<kb_id>:files
type RedisStore struct {
	rdb *redis.Client
}

var _ Store = (*RedisStore)(nil)

func NewRedisStore(addr string) (*RedisStore, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, errors.New("metering: redis address is empty")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &RedisStore{rdb: rdb}, nil
}

func meteringKey(tenantID string) string { return "nexusflow:metering:" + tenantID }

func (r *RedisStore) AddStorage(ctx context.Context, tenantID, kbID string, bytes, files int64) error {
	pipe := r.rdb.TxPipeline()
	pipe.HIncrBy(ctx, meteringKey(tenantID), "kb:"+kbID+":storage", bytes)
	pipe.HIncrBy(ctx, meteringKey(tenantID), "kb:"+kbID+":files", files)
	_, err := pipe.Exec(ctx)
	return err
}

func (r *RedisStore) AddQuery(ctx context.Context, tenantID string, count int64) error {
	return r.rdb.HIncrBy(ctx, meteringKey(tenantID), "queries", count).Err()
}

func (r *RedisStore) Usage(ctx context.Context, tenantID string) (Usage, error) {
	out := Usage{TenantID: tenantID, KnowledgeBases: []KBUsage{}}
	vals, err := r.rdb.HGetAll(ctx, meteringKey(tenantID)).Result()
	if err != nil {
		return out, err
	}
	if len(vals) == 0 {
		return out, nil
	}
	out.QueryCount = parseCount(vals["queries"])

	kbs := make(map[string]*KBUsage)
	for field, raw := range vals {
		if !strings.HasPrefix(field, "kb:") {
			continue
		}
		rest := strings.TrimPrefix(field, "kb:")
		idx := strings.LastIndex(rest, ":")
		if idx <= 0 {
			continue
		}
		kbID, metric := rest[:idx], rest[idx+1:]
		entry := kbs[kbID]
		if entry == nil {
			entry = &KBUsage{KBID: kbID}
			kbs[kbID] = entry
		}
		switch metric {
		case "storage":
			entry.StorageBytes = parseCount(raw)
		case "files":
			entry.FileCount = parseCount(raw)
		}
	}
	for _, entry := range kbs {
		out.StorageBytes += entry.StorageBytes
		out.FileCount += entry.FileCount
		out.KnowledgeBases = append(out.KnowledgeBases, *entry)
	}
	sort.Slice(out.KnowledgeBases, func(i, j int) bool {
		if out.KnowledgeBases[i].StorageBytes != out.KnowledgeBases[j].StorageBytes {
			return out.KnowledgeBases[i].StorageBytes > out.KnowledgeBases[j].StorageBytes
		}
		return out.KnowledgeBases[i].KBID < out.KnowledgeBases[j].KBID
	})
	return out, nil
}

func parseCount(raw string) int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
