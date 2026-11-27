package config

import (
	"net/url"
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
	AuthGRPCAddr       string
	UserGRPCAddr       string
	WorkflowHTTPAddr   string
	// WorkflowTarget is the workflow engine base URL used by the gateway proxy.
	WorkflowTarget string
	// WorkflowServiceToken authenticates gateway/agent calls to the engine.
	WorkflowServiceToken string
	// WorkflowAgentNode registers the DAG -> Agent Hub node.
	WorkflowAgentNode bool
	// WorkflowHITL registers the human_approval node and run persistence.
	WorkflowHITL bool
	// AgentHubURL/AgentHubToken point the agent node at POST /v1/call.
	AgentHubURL     string
	AgentHubToken   string
	AgentHubTimeout time.Duration

	// Downstream endpoints used by the gateway / workers.
	IngestionTarget string
	QueryTarget     string
	SandboxTarget   string
	AuthTarget      string
	UserTarget      string
	MinerUTarget    string
	MinerUAPIKey    string
	MinerUTier      string
	RerankURL       string
	RerankModel     string
	// RerankProtocol selects the wire format: generic (bge/Cohere style) or
	// dashscope (Aliyun text-rerank). RerankAPIKey is only sent for dashscope.
	RerankProtocol string
	RerankAPIKey   string
	RerankPool     int
	RerankTimeout  time.Duration
	// RAGAgentURL is the Python RAG chat service (LangGraph) base URL.
	RAGAgentURL string

	StateDriver string
	RedisAddr   string
	MetricsAddr string

	// IdentityDriver selects the user/role store: memory | redis | postgres.
	IdentityDriver string
	// IdentityDatabaseURL is the PostgreSQL DSN used when IdentityDriver is postgres.
	IdentityDatabaseURL string

	// Feedback persistence: memory | postgres. Postgres keeps user ratings
	// and implicit signals across restarts.
	FeedbackDriver      string
	FeedbackDatabaseURL string

	ObjectStoreDir  string
	ObjectStoreBase string
	// ObjectStoreAllowedHosts restricts presigned upload URLs to trusted hosts
	// (SSRF guard). Empty means only the ObjectStoreBase host is allowed.
	ObjectStoreAllowedHosts []string

	JWTSecret      string
	AllowAnonymous bool
	// TokenBlacklistDriver selects the JWT revocation store: memory | redis.
	// Redis shares revocations between the gateway and auth-service.
	TokenBlacklistDriver string

	// Identity: authentication and RBAC.
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	AllowSelfRegister bool
	SeedTenantID      string
	SeedAdminUsername string
	SeedAdminPassword string

	SandboxBackend string
	// SandboxAllowLocal permits the non-isolated local backend in prod; it is
	// a development convenience, not a security boundary.
	SandboxAllowLocal bool
	SandboxWorkDir    string
	OpenSandboxURL    string
	OpenSandboxToken  string
	// OpenSandboxPythonImage / OpenSandboxNodeImage back bare "python"/"node"
	// runtimes; versioned runtimes such as "python:3.11" are used as images.
	OpenSandboxPythonImage string
	OpenSandboxNodeImage   string

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

	// NL2SQL workflow node: read-only structured queries over Postgres. The DSN
	// is intentionally separate from the identity/feedback databases.
	NL2SQLDatabaseURL string
	NL2SQLMaxRows     int
	NL2SQLTimeout     time.Duration
	// NL2SQLSchema is a short table/column description injected into the
	// question -> SQL prompt.
	NL2SQLSchema     string
	NL2SQLLLMBaseURL string
	NL2SQLLLMAPIKey  string
	NL2SQLModel      string

	// Metering: tenant storage/query counters and their quotas.
	MeteringDriver          string
	QuotaMaxStorageBytes    int64
	QuotaMaxFiles           int64
	QuotaMaxQueriesPerMonth int64
}

func Load() Config {
	return Config{
		Env: env("APP_ENV", "dev"),

		GatewayHTTPAddr:      env("GATEWAY_HTTP_ADDR", ":8080"),
		IngestionGRPCAddr:    env("INGESTION_GRPC_ADDR", ":9095"),
		ProcessingGRPCAddr:   env("PROCESSING_GRPC_ADDR", ":9092"),
		SandboxGRPCAddr:      env("SANDBOX_GRPC_ADDR", ":9093"),
		QueryGRPCAddr:        env("QUERY_GRPC_ADDR", ":9094"),
		AuthGRPCAddr:         env("AUTH_GRPC_ADDR", ":9096"),
		UserGRPCAddr:         env("USER_GRPC_ADDR", ":9097"),
		WorkflowHTTPAddr:     env("WORKFLOW_HTTP_ADDR", ":8081"),
		WorkflowTarget:       env("WORKFLOW_TARGET", "http://127.0.0.1:8081"),
		WorkflowServiceToken: env("WORKFLOW_SERVICE_TOKEN", ""),
		WorkflowAgentNode:    envBool("WORKFLOW_AGENT_NODE", false),
		WorkflowHITL:         envBool("HITL_ENABLED", false),
		AgentHubURL:          env("AGENT_HUB_URL", "http://127.0.0.1:8001"),
		AgentHubToken:        env("AGENT_HUB_TOKEN", ""),
		AgentHubTimeout:      envDuration("AGENT_HUB_TIMEOUT", 30*time.Second),

		IngestionTarget: env("INGESTION_TARGET", "127.0.0.1:9095"),
		QueryTarget:     env("QUERY_TARGET", "127.0.0.1:9094"),
		SandboxTarget:   env("SANDBOX_TARGET", "127.0.0.1:9093"),
		AuthTarget:      env("AUTH_TARGET", "127.0.0.1:9096"),
		UserTarget:      env("USER_TARGET", "127.0.0.1:9097"),
		MinerUTarget:    env("MINERU_TARGET", ""),
		MinerUAPIKey:    env("MINERU_API_KEY", ""),
		MinerUTier:      env("MINERU_TIER", "standard"),
		RerankURL:       env("RERANK_URL", ""),
		RerankModel:     env("RERANK_MODEL", "bge-reranker-v2-m3"),
		RerankProtocol:  env("RERANK_PROTOCOL", "generic"),
		RerankAPIKey:    env("RERANK_API_KEY", ""),
		RerankPool:      envInt("RERANK_POOL", 0),
		RerankTimeout:   envDuration("RERANK_TIMEOUT", 8*time.Second),
		RAGAgentURL:     env("RAG_AGENT_URL", "http://127.0.0.1:8001"),

		StateDriver:         env("STATE_DRIVER", "memory"),
		RedisAddr:           env("REDIS_ADDR", "127.0.0.1:6379"),
		MetricsAddr:         env("METRICS_ADDR", ""),
		IdentityDriver:      env("IDENTITY_DRIVER", "memory"),
		IdentityDatabaseURL: env("IDENTITY_DATABASE_URL", ""),

		FeedbackDriver:      env("FEEDBACK_DRIVER", "memory"),
		FeedbackDatabaseURL: env("FEEDBACK_DATABASE_URL", ""),

		ObjectStoreDir:          env("OBJECT_STORE_DIR", "/tmp/nexusflow-objects"),
		ObjectStoreBase:         env("OBJECT_STORE_BASE", "http://127.0.0.1:9000/nexusflow"),
		ObjectStoreAllowedHosts: objectStoreHosts(env("OBJECT_STORE_ALLOWED_HOSTS", ""), env("OBJECT_STORE_BASE", "http://127.0.0.1:9000/nexusflow")),

		JWTSecret:      env("JWT_SECRET", "nexusflow-dev-secret"),
		AllowAnonymous: envBool("ALLOW_ANONYMOUS", true),

		TokenBlacklistDriver: env("TOKEN_BLACKLIST_DRIVER", "memory"),

		AccessTokenTTL:    envDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL:   envDuration("REFRESH_TOKEN_TTL", 7*24*time.Hour),
		AllowSelfRegister: envBool("ALLOW_SELF_REGISTER", true),
		SeedTenantID:      env("SEED_TENANT_ID", "default"),
		SeedAdminUsername: env("SEED_ADMIN_USERNAME", "admin"),
		SeedAdminPassword: env("SEED_ADMIN_PASSWORD", "admin123"),

		SandboxBackend:         env("SANDBOX_BACKEND", "local"),
		SandboxAllowLocal:      envBool("SANDBOX_ALLOW_LOCAL", false),
		SandboxWorkDir:         env("SANDBOX_WORK_DIR", "/tmp/nexusflow-sandboxes"),
		OpenSandboxURL:         env("OPENSANDBOX_URL", ""),
		OpenSandboxToken:       env("OPENSANDBOX_TOKEN", ""),
		OpenSandboxPythonImage: env("OPENSANDBOX_PYTHON_IMAGE", "python:3.12"),
		OpenSandboxNodeImage:   env("OPENSANDBOX_NODE_IMAGE", "node:18"),

		VectorStoreURL: env("VECTOR_STORE_URL", "http://127.0.0.1:6333"),
		EmbeddingModel: env("EMBEDDING_MODEL", "text-embedding-3-large"),
		LlamaIndexURL:  env("LLAMAINDEX_URL", ""),

		QueueDriver:        env("QUEUE_DRIVER", "memory"),
		QueueURL:           env("QUEUE_URL", "amqp://guest:guest@127.0.0.1:5672/"),
		QueueTopicUploaded: env("QUEUE_TOPIC_UPLOADED", "file.uploaded"),
		QueueTopicParsed:   env("QUEUE_TOPIC_PARSED", "file.parsed"),
		QueueTopicDLQ:      env("QUEUE_TOPIC_DLQ", "task.dlq"),

		ParseMaxAttempts:  envInt("PARSE_MAX_ATTEMPTS", 3),
		ParsePollInterval: envDuration("PARSE_POLL_INTERVAL", 2*time.Second),

		NL2SQLDatabaseURL: env("NL2SQL_DATABASE_URL", ""),
		NL2SQLMaxRows:     envInt("NL2SQL_MAX_ROWS", 200),
		NL2SQLTimeout:     envDuration("NL2SQL_TIMEOUT", 5*time.Second),
		NL2SQLSchema:      env("NL2SQL_SCHEMA", ""),
		NL2SQLLLMBaseURL:  env("NL2SQL_LLM_BASE_URL", ""),
		NL2SQLLLMAPIKey:   env("NL2SQL_LLM_API_KEY", ""),
		NL2SQLModel:       env("NL2SQL_LLM_MODEL", "gpt-4o-mini"),

		MeteringDriver:          env("METERING_DRIVER", "memory"),
		QuotaMaxStorageBytes:    envInt64("QUOTA_MAX_STORAGE_BYTES", 10<<30),
		QuotaMaxFiles:           envInt64("QUOTA_MAX_FILES", 5000),
		QuotaMaxQueriesPerMonth: envInt64("QUOTA_MAX_QUERIES", 100000),
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// objectStoreHosts returns the allowlisted hosts for presigned upload URLs. An
// explicit comma-separated list wins; otherwise the ObjectStoreBase host is used.
func objectStoreHosts(csv, base string) []string {
	if strings.TrimSpace(csv) != "" {
		var out []string
		for _, h := range strings.Split(csv, ",") {
			if h = strings.TrimSpace(h); h != "" {
				out = append(out, h)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return []string{u.Host}
	}
	return nil
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

func envInt64(key string, def int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
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
