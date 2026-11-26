package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// QueueLength backs the K8s HPA custom metric described in the design doc.
var QueueLength = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "nexusflow_queue_length",
	Help: "Number of pending messages per topic.",
}, []string{"service", "topic"})

// ProcessingLatency tracks per-stage processing latency.
var ProcessingLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "nexusflow_processing_latency_seconds",
	Help:    "Processing latency by stage.",
	Buckets: prometheus.DefBuckets,
}, []string{"service", "stage"})

func Handler() http.Handler {
	return promhttp.Handler()
}
