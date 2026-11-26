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

func (r *RabbitMQ) declare(topic string) error {
	_, err := r.ch.QueueDeclare(topic, true, false, false, false, nil)
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

func (r *RabbitMQ) Subscribe(topic string, handler Handler) {
	go func() {
		r.mu.Lock()
		if err := r.declare(topic); err != nil {
			r.mu.Unlock()
			r.logger.Error("declare queue failed", "topic", topic, "error", err)
			return
		}
		deliveries, err := r.ch.Consume(topic, "", false, false, false, false, nil)
		r.mu.Unlock()
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
			if err := handler(context.Background(), msg); err != nil {
				r.logger.Error("handler failed, requeueing", "topic", topic, "error", err)
				_ = delivery.Nack(false, true)
				continue
			}
			_ = delivery.Ack(false)
		}
	}()
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
