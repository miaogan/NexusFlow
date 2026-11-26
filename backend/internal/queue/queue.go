package queue

import (
	"context"
	"sync"
)

// Message is the envelope exchanged between pipeline stages.
type Message struct {
	Topic   string
	Key     string
	Payload []byte
	Headers map[string]string
}

type Handler func(ctx context.Context, msg Message) error

type Publisher interface {
	Publish(ctx context.Context, msg Message) error
}

// Subscriber is implemented by real brokers (RabbitMQ / Kafka) in production.
type Subscriber interface {
	Subscribe(topic string, handler Handler)
}

// MemoryBus is an in-process broker used for local development and tests.
// It preserves the topic-based decoupling of the production design.
type MemoryBus struct {
	mu   sync.RWMutex
	subs map[string][]Handler
	wg   sync.WaitGroup
}

func NewMemoryBus() *MemoryBus {
	return &MemoryBus{subs: make(map[string][]Handler)}
}

func (b *MemoryBus) Publish(_ context.Context, msg Message) error {
	b.mu.RLock()
	handlers := append([]Handler(nil), b.subs[msg.Topic]...)
	b.mu.RUnlock()
	for _, h := range handlers {
		h := h
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			_ = h(context.Background(), msg)
		}()
	}
	return nil
}

func (b *MemoryBus) Subscribe(topic string, handler Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], handler)
}

// Close waits for in-flight handlers to finish.
func (b *MemoryBus) Close() error {
	b.wg.Wait()
	return nil
}
