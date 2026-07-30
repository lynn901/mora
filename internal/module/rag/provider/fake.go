package provider

import (
	"context"
	"errors"
	"hash/fnv"
)

// FakeProvider is a deterministic, dependency-free EmbeddingProvider for tests
// and mock-first integration before real TEI/Ollama is wired. It produces a
// fixed-dimension vector derived from the text hash so identical text yields
// identical vectors (useful for Dense-search smoke tests).
type FakeProvider struct {
	Dim         int
	Unavailable bool
}

func NewFakeProvider(dim int) *FakeProvider { return &FakeProvider{Dim: dim} }

func (f *FakeProvider) Name() string   { return "fake" }
func (f *FakeProvider) Dimension() int { return f.Dim }

func (f *FakeProvider) Embed(ctx context.Context, texts []string, instruction string) ([][]float32, error) {
	if f.Unavailable {
		return nil, &ErrProviderUnavailable{Cause: errors.New("fake provider set unavailable")}
	}
	out := make([][]float32, len(texts))
	for i, t := range applyInstruction(texts, instruction) {
		out[i] = fakeVec(t, f.Dim)
	}
	return out, nil
}

func (f *FakeProvider) HealthCheck(ctx context.Context) error {
	if f.Unavailable {
		return &ErrProviderUnavailable{Cause: errors.New("unavailable")}
	}
	return nil
}

// fakeVec derives a deterministic dim-length unit-ish vector from the text.
// Words raise the corresponding bucket so semantically overlapping text gets
// overlapping dimensions — enough for RRF/RBAC tests without a real model.
func fakeVec(text string, dim int) []float32 {
	v := make([]float32, dim)
	h := fnv.New64a()
	h.Write([]byte(text))
	seed := h.Sum64()
	for i := 0; i < dim; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		v[i] = float32(int32(seed>>33)) / float32(1<<31)
	}
	// boost buckets for each word so shared words correlate.
	for _, w := range splitWords(text) {
		b := wordBucket(w, dim)
		v[b] += 0.5
	}
	// normalize (cosine distance ignores magnitude)
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm > 0 {
		inv := 1.0 / norm
		for i := range v {
			v[i] = float32(float64(v[i]) * inv)
		}
	}
	return v
}

// FakeReranker re-scores by token overlap (jaccard) — deterministic for tests.
type FakeReranker struct{ Unavailable bool }

func (f *FakeReranker) Rerank(ctx context.Context, query string, docs []string) ([]ScoredDoc, error) {
	if f.Unavailable {
		return nil, &ErrProviderUnavailable{Cause: errors.New("reranker unavailable")}
	}
	q := wordSet(query)
	out := make([]ScoredDoc, len(docs))
	for i, d := range docs {
		ds := wordSet(d)
		out[i] = ScoredDoc{Index: i, Score: float32(jaccard(q, ds))}
	}
	sortScoredDesc(out)
	return out, nil
}

func (f *FakeReranker) HealthCheck(ctx context.Context) error {
	if f.Unavailable {
		return &ErrProviderUnavailable{Cause: errors.New("unavailable")}
	}
	return nil
}
