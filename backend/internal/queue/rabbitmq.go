package queue

import (
	"context"
	"log/slog"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitMQ is the production broker implementation. It declares durable
// queues per topic so that stages stay decoupled and survive restarts.
type RabbitMQ struct {
	url    string
	logger *slog.Logger

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

const (
	consumerPrefetch   = 10
	maxDeliveryRetries = 5
)

func NewRabbitMQ(url string, logger *slog.Logger) (*RabbitMQ, error) {
	r := &RabbitMQ{url: url, logger: logger}
	if err := r.connect(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RabbitMQ) connect() error {
	conn, err := amqp.Dial(r.url)
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return err
	}
	r.conn = conn
	r.ch = ch
	return nil
}

func (r *RabbitMQ) declare(topic string) error { return declareOn(r.ch, topic) }

func declareOn(ch *amqp.Channel, topic string) error {
	_, err := ch.QueueDeclare(topic, true, false, false, false, nil)
	return err
}

func (r *RabbitMQ) Publish(ctx context.Context, msg Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.declare(msg.Topic); err != nil {
		return err
	}
	headers := amqp.Table{}
	for k, v := range msg.Headers {
		headers[k] = v
	}
	return r.ch.PublishWithContext(ctx, "", msg.Topic, false, false, amqp.Publishing{
		ContentType: "application/json",
		MessageId:   msg.Key,
		Headers:     headers,
		Body:        msg.Payload,
	})
}

// Subscribe consumes a topic on a dedicated channel. Each consumer gets its own
// channel because amqp.Channel is not safe for concurrent use; the publish
// channel guarded by r.mu is never used for Ack/Nack.
func (r *RabbitMQ) Subscribe(topic string, handler Handler) {
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	if conn == nil {
		r.logger.Error("subscribe before connect", "topic", topic)
		return
	}

	go func() {
		ch, err := conn.Channel()
		if err != nil {
			r.logger.Error("open consumer channel failed", "topic", topic, "error", err)
			return
		}
		defer ch.Close()

		if err := declareOn(ch, topic); err != nil {
			r.logger.Error("declare queue failed", "topic", topic, "error", err)
			return
		}
		if err := ch.Qos(consumerPrefetch, 0, false); err != nil {
			r.logger.Warn("set qos failed", "topic", topic, "error", err)
		}
		deliveries, err := ch.Consume(topic, "", false, false, false, false, nil)
		if err != nil {
			r.logger.Error("consume failed", "topic", topic, "error", err)
			return
		}

		for delivery := range deliveries {
			msg := Message{
				Topic:   topic,
				Key:     delivery.MessageId,
				Payload: delivery.Body,
				Headers: map[string]string{},
			}
			for k, v := range delivery.Headers {
				if s, ok := v.(string); ok {
					msg.Headers[k] = s
				}
			}

			if err := handler(context.Background(), msg); err == nil {
				_ = delivery.Ack(false)
				continue
			} else {
				retries := retryCount(delivery.Headers["x-retry"])
				headers := cloneHeaders(delivery.Headers)
				headers["x-error"] = err.Error()

				if retries >= maxDeliveryRetries {
					// Poison message: dead-letter instead of requeueing forever.
					r.logger.Error("message exceeded retries, dead-lettering",
						"topic", topic, "error", err, "retries", retries)
					if perr := r.publishOn(ch, topic+".dlq", toPublishing(delivery, headers)); perr != nil {
						r.logger.Error("dead-letter publish failed", "topic", topic, "error", perr)
						_ = delivery.Nack(false, true)
						continue
					}
					_ = delivery.Ack(false)
					continue
				}

				headers["x-retry"] = int32(retries + 1)
				r.logger.Warn("handler failed, retrying", "topic", topic, "error", err, "retry", retries+1)
				if perr := r.publishOn(ch, topic, toPublishing(delivery, headers)); perr != nil {
					_ = delivery.Nack(false, true) // fall back to broker requeue
					continue
				}
				_ = delivery.Ack(false)
			}
		}
	}()
}

func (r *RabbitMQ) publishOn(ch *amqp.Channel, topic string, pub amqp.Publishing) error {
	if err := declareOn(ch, topic); err != nil {
		return err
	}
	return ch.PublishWithContext(context.Background(), "", topic, false, false, pub)
}

func toPublishing(d amqp.Delivery, headers amqp.Table) amqp.Publishing {
	return amqp.Publishing{
		ContentType:  d.ContentType,
		MessageId:    d.MessageId,
		Headers:      headers,
		Body:         d.Body,
		DeliveryMode: d.DeliveryMode,
	}
}

func cloneHeaders(in amqp.Table) amqp.Table {
	out := amqp.Table{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func retryCount(v any) int {
	switch n := v.(type) {
	case int32:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func (r *RabbitMQ) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ch != nil {
		_ = r.ch.Close()
	}
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}
