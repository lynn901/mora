// Package provider implements the Embedding & Reranker provider abstraction
// (05-rag-pipeline-design.md §5). TEI is the primary backend, Ollama the
// lightweight alternative; both are pluggable behind EmbeddingProvider.
package provider

import (
	"context"
	"fmt"
)

// EmbeddingProvider turns text into vectors. Instruction distinguishes the
// query vs document encoding for Instruction-Aware models (Qwen3-Embedding).
type EmbeddingProvider interface {
	// Embed batch-encodes texts with the given instruction prefix ("" = none).
	Embed(ctx context.Context, texts []string, instruction string) ([][]float32, error)
	Dimension() int
	HealthCheck(ctx context.Context) error
	Name() string
}

// RerankerProvider re-scores (query, doc) pairs with a Cross-Encoder.
type RerankerProvider interface {
	Rerank(ctx context.Context, query string, docs []string) ([]ScoredDoc, error)
	HealthCheck(ctx context.Context) error
}

// ScoredDoc is one reranked result: the index into the input docs and its score.
type ScoredDoc struct {
	Index int
	Score float32
}

// ErrProviderUnavailable signals the model backend is down; the pipeline treats
// this as retryable (queue, don't drop) and search degrades to pure BM25.
type ErrProviderUnavailable struct{ Cause error }

func (e *ErrProviderUnavailable) Error() string {
	return fmt.Sprintf("embedding provider unavailable: %v", e.Cause)
}
func (e *ErrProviderUnavailable) Unwrap() error { return e.Cause }

// applyInstruction prepends the instruction prefix to every text (no-op if empty).
func applyInstruction(texts []string, instruction string) []string {
	if instruction == "" {
		return texts
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = instruction + " " + t
	}
	return out
}
