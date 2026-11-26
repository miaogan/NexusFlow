package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the process-level configuration shared by every service.
// Each binary only reads the fields it needs, which keeps deployment simple.
type Config struct {
	Env string

	GatewayHTTPAddr    string
	IngestionGRPCAddr  string
	ProcessingGRPCAddr string
	SandboxGRPCAddr    string
	QueryGRPCAddr      string
	WorkflowHTTPAddr   string

	// Downstream endpoints used by the gateway / workers.
	IngestionTarget string
	QueryTarget     string
	MinerUTarget    string
	RerankURL       string
	RerankModel     string

	StateDriver string
	RedisAddr   string
	MetricsAddr string

	ObjectStoreDir  string
	ObjectStoreBase string

	JWTSecret      string
	AllowAnonymous bool

	SandboxBackend   string
	SandboxWorkDir   string
	OpenSandboxURL   string
	OpenSandboxToken string

	VectorStoreURL string
	EmbeddingModel string
	LlamaIndexURL  string

	QueueDriver        string
	QueueURL           string
	QueueTopicUploaded string
	QueueTopicParsed   string
	QueueTopicDLQ      string

	ParseMaxAttempts  int
	ParsePollInterval time.Duration
}

func Load() Config {
	return Config{
		Env: env("APP_ENV", "dev"),

		GatewayHTTPAddr:    env("GATEWAY_HTTP_ADDR", ":8080"),
		IngestionGRPCAddr:  env("INGESTION_GRPC_ADDR", ":9095"),
		ProcessingGRPCAddr: env("PROCESSING_GRPC_ADDR", ":9092"),
		SandboxGRPCAddr:    env("SANDBOX_GRPC_ADDR", ":9093"),
		QueryGRPCAddr:      env("QUERY_GRPC_ADDR", ":9094"),
		WorkflowHTTPAddr:   env("WORKFLOW_HTTP_ADDR", ":8081"),

		IngestionTarget: env("INGESTION_TARGET", "127.0.0.1:9095"),
		QueryTarget:     env("QUERY_TARGET", "127.0.0.1:9094"),
		MinerUTarget:    env("MINERU_TARGET", ""),
		RerankURL:       env("RERANK_URL", ""),
		RerankModel:     env("RERANK_MODEL", "bge-reranker-v2-m3"),

		StateDriver: env("STATE_DRIVER", "memory"),
		RedisAddr:   env("REDIS_ADDR", "127.0.0.1:6379"),
		MetricsAddr: env("METRICS_ADDR", ""),

		ObjectStoreDir:  env("OBJECT_STORE_DIR", "/tmp/nexusflow-objects"),
		ObjectStoreBase: env("OBJECT_STORE_BASE", "http://127.0.0.1:9000/nexusflow"),

		JWTSecret:      env("JWT_SECRET", "nexusflow-dev-secret"),
		AllowAnonymous: envBool("ALLOW_ANONYMOUS", true),

		SandboxBackend:   env("SANDBOX_BACKEND", "local"),
		SandboxWorkDir:   env("SANDBOX_WORK_DIR", "/tmp/nexusflow-sandboxes"),
		OpenSandboxURL:   env("OPENSANDBOX_URL", ""),
		OpenSandboxToken: env("OPENSANDBOX_TOKEN", ""),

		VectorStoreURL: env("VECTOR_STORE_URL", "http://127.0.0.1:6333"),
		EmbeddingModel: env("EMBEDDING_MODEL", "text-embedding-3-large"),
		LlamaIndexURL:  env("LLAMAINDEX_URL", "http://127.0.0.1:8000"),

		QueueDriver:        env("QUEUE_DRIVER", "memory"),
		QueueURL:           env("QUEUE_URL", "amqp://guest:guest@127.0.0.1:5672/"),
		QueueTopicUploaded: env("QUEUE_TOPIC_UPLOADED", "file.uploaded"),
		QueueTopicParsed:   env("QUEUE_TOPIC_PARSED", "file.parsed"),
		QueueTopicDLQ:      env("QUEUE_TOPIC_DLQ", "task.dlq"),

		ParseMaxAttempts:  envInt("PARSE_MAX_ATTEMPTS", 3),
		ParsePollInterval: envDuration("PARSE_POLL_INTERVAL", 2*time.Second),
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
