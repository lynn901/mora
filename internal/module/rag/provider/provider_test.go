package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTEIProvider_EmbedAndInstruction(t *testing.T) {
	var gotInputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body struct {
			Inputs []string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotInputs = body.Inputs
		// return 2x3 vectors
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([][]float32{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}})
	}))
	defer srv.Close()
	p := NewTEIProvider(srv.URL, "Qwen3-Embedding", 3)
	vecs, err := p.Embed(context.Background(), []string{"a", "b"}, "passage:")
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 {
		t.Fatalf("unexpected vectors: %v", vecs)
	}
	if gotInputs[0] != "passage: a" {
		t.Errorf("instruction not applied: %v", gotInputs)
	}
}

func TestTEIProvider_DimensionMismatchRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([][]float32{{0.1, 0.2}}) // dim 2, expected 3
	}))
	defer srv.Close()
	p := NewTEIProvider(srv.URL, "m", 3)
	_, err := p.Embed(context.Background(), []string{"a"}, "")
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}

func TestTEIProvider_UnavailableReturnsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := NewTEIProvider(srv.URL, "m", 3)
	_, err := p.Embed(context.Background(), []string{"a"}, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*ErrProviderUnavailable); !ok {
		t.Errorf("expected ErrProviderUnavailable, got %T", err)
	}
}

func TestTEIProvider_Rerank(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]ScoredDoc{{Index: 1, Score: 0.9}, {Index: 0, Score: 0.2}})
	}))
	defer srv.Close()
	p := NewTEIProvider(srv.URL, "rerank", 3)
	scored, err := p.Rerank(context.Background(), "q", []string{"d0", "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if scored[0].Index != 1 || scored[0].Score != 0.9 {
		t.Errorf("rerank not sorted desc: %+v", scored)
	}
}

func TestOllamaProvider_BatchThenFallback(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embed":
			// simulate older Ollama without batch: 404
			calls++
			w.WriteHeader(http.StatusNotFound)
		case "/api/embeddings":
			calls++
			_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{0.1, 0.2}})
		}
	}))
	defer srv.Close()
	p := NewOllamaProvider(srv.URL, "qwen3", 2)
	vecs, err := p.Embed(context.Background(), []string{"a", "b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 2 {
		t.Fatalf("unexpected: %v", vecs)
	}
	if calls != 3 { // 1 batch attempt (404) + 2 single
		t.Errorf("expected fallback to single-prompt, calls=%d", calls)
	}
}

func TestFakeProvider_DeterministicAndUnavailable(t *testing.T) {
	p := NewFakeProvider(8)
	v1, _ := p.Embed(context.Background(), []string{"hello world"}, "")
	v2, _ := p.Embed(context.Background(), []string{"hello world"}, "")
	if len(v1[0]) != 8 || v1[0][0] != v2[0][0] {
		t.Errorf("fake provider not deterministic")
	}
	p.Unavailable = true
	if _, err := p.Embed(context.Background(), []string{"x"}, ""); err == nil {
		t.Error("expected unavailable error")
	}
	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("expected health check failure")
	}
}

func TestFakeReranker_Overlap(t *testing.T) {
	r := &FakeReranker{}
	scored, err := r.Rerank(context.Background(), "api design pagination", []string{"api design", "random text"})
	if err != nil {
		t.Fatal(err)
	}
	if scored[0].Index != 0 {
		t.Errorf("expected doc0 (overlap) first, got %+v", scored)
	}
}
