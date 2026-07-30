// Package ragwiring wires RAG infra components: the default Embedding/Reranker
// provider factory (TEI primary, Ollama fallback) and the Valkey idempotency
// store. It is the composition root used by cmd/rag-worker.
package ragwiring

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/rag"
	"github.com/wiki/wiki-backend/internal/module/rag/provider"
)

// DefaultProviderFactory builds TEI or Ollama providers per model config.
// TEI is primary; Ollama is the lightweight alternative (05 §5.2).
type DefaultProviderFactory struct {
	TEIURL       string
	OllamaURL    string
	RerankModel  string // TEI reranker model name (e.g. BAAI/bge-reranker-large)
}

func (f *DefaultProviderFactory) For(ctx context.Context, m domain.EmbeddingModel) (provider.EmbeddingProvider, error) {
	switch m.Provider {
	case "tei", "TEI":
		return provider.NewTEIProvider(f.TEIURL, m.ModelName, m.Dimension), nil
	case "ollama", "Ollama":
		return provider.NewOllamaProvider(f.OllamaURL, m.ModelName, m.Dimension), nil
	default:
		return provider.NewTEIProvider(f.TEIURL, m.ModelName, m.Dimension), nil
	}
}

func (f *DefaultProviderFactory) Reranker(ctx context.Context) (provider.RerankerProvider, error) {
	if f.RerankModel == "" {
		return nil, nil // rerank disabled until configured
	}
	return provider.NewTEIProvider(f.TEIURL, f.RerankModel, 0), nil
}

// ValkeyIdempotencyStore deduplicates events by event_id with SET NX EX (TTL 24h).
type ValkeyIdempotencyStore struct {
	Rdb *redis.Client
}

func (s *ValkeyIdempotencyStore) MarkSeen(ctx context.Context, eventID string, ttl time.Duration) (bool, error) {
	ok, err := s.Rdb.SetNX(ctx, "rag:seen:"+eventID, "1", ttl).Result()
	if err != nil {
		return false, err
	}
	// ok==true means we just set it (not seen before) → return false (not previously seen).
	return !ok, nil
}

// Compile-time check that the factory satisfies the port.
var _ rag.ProviderFactory = (*DefaultProviderFactory)(nil)
var _ rag.IdempotencyStore = (*ValkeyIdempotencyStore)(nil)
