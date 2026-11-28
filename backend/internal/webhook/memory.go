package webhook

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore keeps subscriptions and delivery logs in process memory.
type MemoryStore struct {
	mu           sync.RWMutex
	subs         map[string]Subscription
	deliveries   map[string]Delivery
	deliveryList []string
}

// NewMemoryStore returns an empty in-memory webhook store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		subs:       make(map[string]Subscription),
		deliveries: make(map[string]Delivery),
	}
}

func (s *MemoryStore) CreateSubscription(_ context.Context, sub Subscription) (Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub.ID == "" {
		sub.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = now
	}
	sub.UpdatedAt = now
	s.subs[sub.ID] = cloneSubscription(sub)
	return cloneSubscription(sub), nil
}

func (s *MemoryStore) ListSubscriptions(_ context.Context, tenantID string) ([]Subscription, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		if sub.TenantID == tenantID {
			out = append(out, cloneSubscription(sub))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) GetSubscription(_ context.Context, tenantID, id string) (Subscription, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subs[id]
	if !ok || sub.TenantID != tenantID {
		return Subscription{}, ErrNotFound
	}
	return cloneSubscription(sub), nil
}

func (s *MemoryStore) DeleteSubscription(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok || sub.TenantID != tenantID {
		return ErrNotFound
	}
	delete(s.subs, id)
	for deliveryID, delivery := range s.deliveries {
		if delivery.WebhookID == id {
			delete(s.deliveries, deliveryID)
		}
	}
	filtered := s.deliveryList[:0]
	for _, deliveryID := range s.deliveryList {
		if _, ok := s.deliveries[deliveryID]; ok {
			filtered = append(filtered, deliveryID)
		}
	}
	s.deliveryList = filtered
	return nil
}

func (s *MemoryStore) ListActive(_ context.Context, tenantID string) ([]Subscription, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		if sub.TenantID == tenantID && sub.IsActive {
			out = append(out, cloneSubscription(sub))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) CreateDelivery(_ context.Context, d Delivery) (Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	if d.Status == "" {
		d.Status = StatusPending
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	s.deliveries[d.ID] = cloneDelivery(d)
	s.deliveryList = append(s.deliveryList, d.ID)
	return cloneDelivery(d), nil
}

func (s *MemoryStore) UpdateDelivery(_ context.Context, d Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deliveries[d.ID]; !ok {
		return ErrNotFound
	}
	s.deliveries[d.ID] = cloneDelivery(d)
	return nil
}

func (s *MemoryStore) ListDeliveries(_ context.Context, tenantID, webhookID string, limit int) ([]Delivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 50
	}
	out := make([]Delivery, 0, limit)
	for i := len(s.deliveryList) - 1; i >= 0 && len(out) < limit; i-- {
		delivery := s.deliveries[s.deliveryList[i]]
		if delivery.TenantID != tenantID || delivery.WebhookID != webhookID {
			continue
		}
		out = append(out, cloneDelivery(delivery))
	}
	return out, nil
}

func (s *MemoryStore) Close() error { return nil }

func cloneSubscription(sub Subscription) Subscription {
	sub.Events = append([]string{}, sub.Events...)
	return sub
}

func cloneDelivery(d Delivery) Delivery {
	d.Payload = append([]byte{}, d.Payload...)
	if d.NextRetryAt != nil {
		next := *d.NextRetryAt
		d.NextRetryAt = &next
	}
	return d
}
