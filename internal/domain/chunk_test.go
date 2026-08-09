package domain

import "testing"

// TestCollectionName_DefaultPrefix verifies the deterministic collection name
// uses the mora_chunks_ prefix. One collection per model; the slug is stable.
func TestCollectionName_DefaultPrefix(t *testing.T) {
	m := EmbeddingModel{Provider: "tei", ModelName: "Qwen3-Embedding-0.6B", Dimension: 1024}
	want := "mora_chunks_tei_qwen3_embedding_0_6b_1024"
	if got := m.CollectionName(); got != want {
		t.Fatalf("CollectionName() = %q, want %q", got, want)
	}
	if got := CollectionPrefix(); got != "mora_chunks_" {
		t.Fatalf("CollectionPrefix() = %q, want %q", got, "mora_chunks_")
	}
}

// TestSetCollectionPrefix verifies that overriding the prefix changes every
// model's collection name, and an empty
// value is ignored so the default stays safe. Restores the default at the end
// so other tests in this package are unaffected.
func TestSetCollectionPrefix(t *testing.T) {
	t.Cleanup(func() { SetCollectionPrefix("mora_chunks_") })

	m := EmbeddingModel{Provider: "tei", ModelName: "all-MiniLM-L6-v2", Dimension: 384}

	SetCollectionPrefix("acme_chunks_")
	if got, want := m.CollectionName(), "acme_chunks_tei_all_minilm_l6_v2_384"; got != want {
		t.Fatalf("after SetCollectionPrefix: CollectionName() = %q, want %q", got, want)
	}

	// empty value must be ignored (keep last good prefix, never produce a bare name)
	SetCollectionPrefix("   ")
	if got := m.CollectionName(); !startsWith(got, "acme_chunks_") {
		t.Fatalf("empty prefix ignored: CollectionName() = %q, want %q-prefix", got, "acme_chunks_")
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
