package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TEIProvider talks to HuggingFace Text Embeddings Inference (/embed, /rerank).
// It is the primary backend; supports Qwen3-Embedding and BAAI/bge-reranker.
type TEIProvider struct {
	BaseURL    string
	HTTPClient *http.Client
	ModelName  string
	Dim        int
}

// NewTEIProvider returns a TEI provider. dim is the declared model dimension
// (validated against actual embeddings to prevent dimension-mixing).
func NewTEIProvider(baseURL, modelName string, dim int) *TEIProvider {
	return &TEIProvider{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		ModelName:  modelName,
		Dim:        dim,
	}
}

func (t *TEIProvider) Name() string    { return "tei:" + t.ModelName }
func (t *TEIProvider) Dimension() int  { return t.Dim }
func (t *TEIProvider) model() string   { return t.ModelName }

// Embed calls TEI POST /embed (batch). TEI accepts {"inputs":[...]} and returns
// [[f32],...]. We tolerate both the raw array shape and the {"data":[...]} shape.
func (t *TEIProvider) Embed(ctx context.Context, texts []string, instruction string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"inputs":   applyInstruction(texts, instruction),
		"truncate": true,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return nil, &ErrProviderUnavailable{Cause: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ErrProviderUnavailable{Cause: fmt.Errorf("tei /embed %d: %s", resp.StatusCode, snippet(raw))}
	}
	vecs, err := decodeEmbeddings(raw)
	if err != nil {
		return nil, fmt.Errorf("tei decode: %w", err)
	}
	if err := validateDims(vecs, t.Dim); err != nil {
		return nil, err
	}
	return vecs, nil
}

// Rerank calls TEI POST /rerank (Cross-Encoder). Returns ScoredDoc in input order
// of score desc.
func (t *TEIProvider) Rerank(ctx context.Context, query string, docs []string) ([]ScoredDoc, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(map[string]any{"query": query, "texts": docs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return nil, &ErrProviderUnavailable{Cause: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ErrProviderUnavailable{Cause: fmt.Errorf("tei /rerank %d: %s", resp.StatusCode, snippet(raw))}
	}
	// TEI returns [{"index":0,"score":0.9}, ...]
	var scored []ScoredDoc
	if err := json.Unmarshal(raw, &scored); err != nil {
		// tolerate {"results":[...]}
		var wrap struct {
			Results []ScoredDoc `json:"results"`
		}
		if err2 := json.Unmarshal(raw, &wrap); err2 != nil {
			return nil, fmt.Errorf("tei rerank decode: %w", err)
		}
		scored = wrap.Results
	}
	sortScoredDesc(scored)
	return scored, nil
}

func (t *TEIProvider) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return &ErrProviderUnavailable{Cause: err}
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &ErrProviderUnavailable{Cause: fmt.Errorf("tei /health %d", resp.StatusCode)}
	}
	return nil
}

// --- shared decode helpers ---

// decodeEmbeddings tolerates the two common TEI /embed response shapes.
func decodeEmbeddings(raw []byte) ([][]float32, error) {
	// Shape A: [[f32],...]
	var asArray [][]float32
	if err := json.Unmarshal(raw, &asArray); err == nil {
		return asArray, nil
	}
	// Shape B: {"data":[{"embedding":[...]}]}
	var asObj struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &asObj); err != nil {
		return nil, err
	}
	if len(asObj.Embeddings) > 0 {
		return asObj.Embeddings, nil
	}
	out := make([][]float32, len(asObj.Data))
	for i, d := range asObj.Data {
		out[i] = d.Embedding
	}
	return out, nil
}

func validateDims(vecs [][]float32, expected int) error {
	for i, v := range vecs {
		if len(v) != expected {
			return fmt.Errorf("dimension mismatch: vector %d has %d, expected %d (refuse to mix dimensions)", i, len(v), expected)
		}
	}
	return nil
}

func sortScoredDesc(s []ScoredDoc) {
	// simple insertion sort (rerank candidate sets are small, ≤50)
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j].Score > s[j-1].Score {
			s[j], s[j-1] = s[j-1], s[j]
			j--
		}
	}
}

func snippet(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}
