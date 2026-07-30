// Package pipeline implements the event-driven RAG indexing pipeline
// (05-rag-pipeline-design.md §3): text extraction → chunking → embedding →
// Qdrant write → index-status receipt, with cascade delete/update and
// permission-driven visible_to recompute.
package pipeline

import "time"

// Config tunes the pipeline. Defaults follow PRD F2.1 (chunk 512 / overlap 64).
type Config struct {
	ChunkSize             int           // default 512 tokens
	ChunkOverlap          int           // default 64 tokens
	RespectHeadingBoundary bool         // default true
	MaxChunkSize          int           // default 1024 tokens
	EmbedBatchSize        int           // default 32 (TEI batch /embed)
	CollectionPrefix      string        // default "wiki_chunks"
	// Retry backoff for transient failures (worker-level). 10s/30s/90s by default.
	Backoffs              []time.Duration
	MaxAttempt            int // default 3
}

func DefaultConfig() Config {
	return Config{
		ChunkSize:              512,
		ChunkOverlap:           64,
		RespectHeadingBoundary: true,
		MaxChunkSize:           1024,
		EmbedBatchSize:         32,
		CollectionPrefix:       "wiki_chunks",
		Backoffs:               []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second},
		MaxAttempt:             3,
	}
}

// Backoff returns the wait before the (attempt+1)-th try (0-indexed attempt).
func (c Config) Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(c.Backoffs) {
		if len(c.Backoffs) == 0 {
			return 0
		}
		return c.Backoffs[len(c.Backoffs)-1]
	}
	return c.Backoffs[attempt]
}
