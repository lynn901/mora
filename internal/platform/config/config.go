package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the modular monolith, sourced
// exclusively from env vars (no hardcoded secrets — 01-tech-selection-decision
// §3.4). One struct serves every binary (mora-api, rag-worker, mcp-server);
// each binary reads the subset of fields it needs. Load() is the mora-api
// entrypoint (returns *Config + error); FromEnv()/Default() are the MCP
// entrypoints (return Config).
type Config struct {
	// --- shared ---
	HTTPAddr               string
	DatabaseURL            string // mora-api / rag-worker PG DSN
	PostgresDSN            string // mcp-server PG DSN (alias of DatabaseURL)
	ValkeyURL              string
	QdrantURL              string
	QdrantCollectionPrefix string // Qdrant collection-name prefix (default "mora_chunks_")
	TEIURL                 string
	OllamaURL              string
	RerankerModel          string
	JWTSecret              string
	JWTTTL                 time.Duration
	LogLevel               string

	// --- embedding / search ---
	EmbeddingProvider string
	EmbeddingModel    string
	EmbeddingDim      int
	FTSConfig         string // chinese_zh when zhparser present, else simple

	// --- mora domain ---
	VersionRetentionDays  int
	VersionRetentionCount int
	CollabMaxConcurrent   int
	RateLimitDocPerMin    int
	RateLimitSearchPerMin int
	RateLimitMCPPerMin    int

	// Minio object storage (attachments + parsed source files, 10 §4.2.1)
	MinioEndpoint  string
	MinioAccessKey string
	MinioSecretKey string
	MinioBucket    string
	MinioSecure    bool // true = HTTPS
	MinioRegion    string

	// --- multi-format document parsing (10 §9.2) ---
	MoraParserURL         string // mora-parser sidecar (OCR/VLM/ASR); empty = disabled (P2)
	WhisperURL            string // whisper.cpp ASR server; empty = disabled (P2)
	ParseMaxFileMB        int    // single-file upload cap
	ParseOCRDefaultLang   string // chi_sim+eng
	ParseVLMModel         string // ollama VLM model id, e.g. minicpm-v
	ParseTaskMaxAttempt   int    // parse task retry count
	ParseDeadLetterStream string

	// --- mcp domain ---
	Transport       string // "http" (default) or "stdio"
	MoraAPIURL      string // internal Mora API base URL
	InternalToken   string // INTERNAL_SERVICE_TOKEN for MCP->Mora service auth
	RateLimitRead   int
	RateLimitWrite  int
	SessionTTL      time.Duration
	ProtocolVersion string
	ServerName      string
	ServerVersion   string

	// --- rag worker ---
	ConsumerName string
}

// Load reads configuration from environment variables with safe defaults for
// the mora-api binary. Returns an error only on malformed duration/int values.
func Load() (*Config, error) {
	cfg := &Config{
		HTTPAddr:               getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:            getenv("DATABASE_URL", ""),
		PostgresDSN:            getenv("DATABASE_URL", ""),
		ValkeyURL:              getenv("VALKEY_URL", "redis://localhost:6379"),
		MinioEndpoint:          getenv("MINIO_ENDPOINT", "localhost:9000"),
		MinioAccessKey:         getenv("MINIO_ACCESS_KEY", ""),
		MinioSecretKey:         getenv("MINIO_SECRET_KEY", ""),
		MinioBucket:            getenv("MINIO_BUCKET", "mora"),
		MinioSecure:            os.Getenv("MINIO_SECURE") == "true",
		MinioRegion:            getenv("MINIO_REGION", "us-east-1"),
		MoraParserURL:          getenv("MORA_PARSER_URL", ""),
		WhisperURL:             getenv("WHISPER_URL", ""),
		ParseMaxFileMB:         getenvInt("PARSE_MAX_FILE_MB", 100),
		ParseOCRDefaultLang:    getenv("PARSE_OCR_DEFAULT_LANG", "chi_sim+eng"),
		ParseVLMModel:          getenv("PARSE_VLM_MODEL", "minicpm-v"),
		ParseTaskMaxAttempt:    getenvInt("PARSE_TASK_MAX_ATTEMPT", 3),
		ParseDeadLetterStream:  getenv("PARSE_DEAD_LETTER_STREAM", "doc_events:parse_dead"),
		QdrantURL:              getenv("QDRANT_URL", "http://localhost:6333"),
		QdrantCollectionPrefix: getenv("RAG_COLLECTION_PREFIX", "mora_chunks_"),
		TEIURL:                 getenv("TEI_URL", "http://localhost:8080"),
		OllamaURL:              getenv("OLLAMA_URL", "http://localhost:11434"),
		RerankerModel:          getenv("RERANKER_MODEL", ""),
		JWTSecret:              getenv("JWT_SECRET", ""),
		EmbeddingProvider:      getenv("EMBEDDING_PROVIDER", "tei"),
		EmbeddingModel:         getenv("EMBEDDING_MODEL", "sentence-transformers/all-MiniLM-L6-v2"),
		FTSConfig:              getenv("FTS_CONFIG", ""),
		VersionRetentionDays:   getenvInt("VERSION_RETENTION_DAYS", 90),
		VersionRetentionCount:  getenvInt("VERSION_RETENTION_COUNT", 100),
		CollabMaxConcurrent:    getenvInt("COLLAB_MAX_CONCURRENT", 50),
		RateLimitDocPerMin:     getenvInt("RATE_LIMIT_DOC_PER_MIN", 300),
		RateLimitSearchPerMin:  getenvInt("RATE_LIMIT_SEARCH_PER_MIN", 200),
		RateLimitMCPPerMin:     getenvInt("RATE_LIMIT_MCP_PER_MIN", 100),
		Transport:              getenv("MCP_TRANSPORT", "http"),
		MoraAPIURL:             getenv("WIKI_API_URL", "http://localhost:8080"),
		InternalToken:          os.Getenv("INTERNAL_SERVICE_TOKEN"),
		RateLimitRead:          getenvInt("MCP_RATE_LIMIT_READ", 100),
		RateLimitWrite:         getenvInt("MCP_RATE_LIMIT_WRITE", 20),
		ProtocolVersion:        getenv("MCP_PROTOCOL_VERSION", "2025-06-18"),
		ServerName:             getenv("MCP_SERVER_NAME", "mora-mcp"),
		ServerVersion:          getenv("MCP_SERVER_VERSION", "1.0.0"),
		ConsumerName:           getenv("CONSUMER_NAME", "rag-worker-1"),
		LogLevel:               strings.ToLower(getenv("LOG_LEVEL", "info")),
	}
	cfg.EmbeddingDim = getenvInt("EMBEDDING_DIM", 384)
	cfg.PostgresDSN = cfg.DatabaseURL

	ttl, err := time.ParseDuration(getenv("JWT_TTL", "8h"))
	if err != nil {
		return nil, fmt.Errorf("invalid JWT_TTL: %w", err)
	}
	cfg.JWTTTL = ttl
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 30 * time.Minute
	}
	if cfg.JWTSecret == "" {
		// Dev default; production MUST inject JWT_SECRET via env.
		cfg.JWTSecret = "dev-insecure-secret-change-me"
	}
	if cfg.FTSConfig == "" {
		cfg.FTSConfig = "chinese_zh"
	}
	return cfg, nil
}

// Default returns a Config populated with MCP-safe production defaults.
func Default() Config {
	return Config{
		HTTPAddr:               ":8081",
		Transport:              "http",
		MoraAPIURL:             "http://localhost:8080",
		InternalToken:          "",
		PostgresDSN:            "",
		DatabaseURL:            "",
		ValkeyURL:              "",
		QdrantCollectionPrefix: "mora_chunks_",
		RateLimitRead:          100,
		RateLimitWrite:         20,
		SessionTTL:             30 * time.Minute,
		ProtocolVersion:        "2025-06-18",
		ServerName:             "mora-mcp",
		ServerVersion:          "1.0.0",
		LogLevel:               "info",
	}
}

// FromEnv loads a Config from environment variables over MCP defaults, used by
// the mcp-server binary. Never returns an error (falls back to defaults).
func FromEnv() Config {
	c := Default()
	if v := getenv("MCP_HTTP_ADDR", ""); v != "" {
		c.HTTPAddr = v
	} else if v := getenv("HTTP_ADDR", ""); v != "" {
		c.HTTPAddr = v
	}
	if v := os.Getenv("MCP_TRANSPORT"); v != "" {
		c.Transport = strings.ToLower(v)
	}
	if v := getenv("WIKI_API_URL", ""); v != "" {
		c.MoraAPIURL = strings.TrimRight(v, "/")
	}
	c.InternalToken = os.Getenv("INTERNAL_SERVICE_TOKEN")
	c.DatabaseURL = os.Getenv("DATABASE_URL")
	c.PostgresDSN = c.DatabaseURL
	c.ValkeyURL = os.Getenv("VALKEY_URL")
	c.QdrantURL = getenv("QDRANT_URL", "http://localhost:6333")
	c.QdrantCollectionPrefix = getenv("RAG_COLLECTION_PREFIX", "mora_chunks_")
	c.TEIURL = getenv("TEI_URL", "http://localhost:8080")
	c.OllamaURL = getenv("OLLAMA_URL", "http://localhost:11434")
	if v := getenvInt("MCP_RATE_LIMIT_READ", 0); v > 0 {
		c.RateLimitRead = v
	}
	if v := getenvInt("MCP_RATE_LIMIT_WRITE", 0); v > 0 {
		c.RateLimitWrite = v
	}
	if v := os.Getenv("MCP_SESSION_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.SessionTTL = d
		}
	}
	if v := getenv("MCP_PROTOCOL_VERSION", ""); v != "" {
		c.ProtocolVersion = v
	}
	if v := getenv("MCP_SERVER_NAME", ""); v != "" {
		c.ServerName = v
	}
	if v := getenv("MCP_SERVER_VERSION", ""); v != "" {
		c.ServerVersion = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		c.LogLevel = strings.ToLower(v)
	}
	return c
}

// String returns a redacted, human-readable representation (no secrets).
func (c Config) String() string {
	return fmt.Sprintf(
		"Config{http=%s transport=%s mora_api=%s pg=%v valkey=%v qdrant=%s tei=%s rl_read=%d rl_write=%d proto=%s server=%s/%s}",
		c.HTTPAddr, c.Transport, c.MoraAPIURL,
		c.DatabaseURL != "", c.ValkeyURL != "",
		c.QdrantURL, c.TEIURL,
		c.RateLimitRead, c.RateLimitWrite,
		c.ProtocolVersion, c.ServerName, c.ServerVersion,
	)
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
