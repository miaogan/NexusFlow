package broker

import (
	"log/slog"

	"github.com/nexusflow/nexusflow/backend/internal/queue"
)

// Broker is both sides of the messaging boundary.
type Broker interface {
	queue.Publisher
	queue.Subscriber
	Close() error
}

// New selects the broker implementation. Memory keeps local development
// single-process; RabbitMQ is used when services run separately.
func New(driver, url string, logger *slog.Logger) (Broker, error) {
	if driver == "rabbitmq" {
		return queue.NewRabbitMQ(url, logger)
	}
	return queue.NewMemoryBus(), nil
}
