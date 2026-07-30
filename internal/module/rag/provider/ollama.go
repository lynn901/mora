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

// OllamaProvider talks to Ollama /api/embeddings (single-prompt) and, when
// available, /api/embed (batch). It is the lightweight alternative to TEI.
type OllamaProvider struct {
	BaseURL    string
	HTTPClient *http.Client
	ModelName  string
	Dim        int
}

func NewOllamaProvider(baseURL, modelName string, dim int) *OllamaProvider {
	return &OllamaProvider{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		ModelName:  modelName,
		Dim:        dim,
	}
}

func (o *OllamaProvider) Name() string   { return "ollama:" + o.ModelName }
func (o *OllamaProvider) Dimension() int { return o.Dim }

// Embed encodes texts via Ollama. Tries the batch /api/embed endpoint first;
// falls back to per-text /api/embeddings (single-prompt) for older Ollama.
func (o *OllamaProvider) Embed(ctx context.Context, texts []string, instruction string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	inputs := applyInstruction(texts, instruction)
	if vecs, err := o.embedBatch(ctx, inputs); err == nil {
		if err := validateDims(vecs, o.Dim); err != nil {
			return nil, err
		}
		return vecs, nil
	}
	// fallback: per-prompt /api/embeddings
	out := make([][]float32, len(inputs))
	for i, p := range inputs {
		v, err := o.embedOne(ctx, p)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	if err := validateDims(out, o.Dim); err != nil {
		return nil, err
	}
	return out, nil
}

func (o *OllamaProvider) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.ModelName, "input": inputs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return nil, &ErrProviderUnavailable{Cause: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ollama /api/embed %d", resp.StatusCode)
	}
	var obj struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Embeddings == nil {
		return nil, fmt.Errorf("ollama batch unavailable")
	}
	return obj.Embeddings, nil
}

func (o *OllamaProvider) embedOne(ctx context.Context, prompt string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.ModelName, "prompt": prompt})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return nil, &ErrProviderUnavailable{Cause: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &ErrProviderUnavailable{Cause: fmt.Errorf("ollama /api/embeddings %d", resp.StatusCode)}
	}
	var obj struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("ollama decode: %w", err)
	}
	return obj.Embedding, nil
}

func (o *OllamaProvider) HealthCheck(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+"/api/tags", nil)
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return &ErrProviderUnavailable{Cause: err}
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &ErrProviderUnavailable{Cause: fmt.Errorf("ollama /api/tags %d", resp.StatusCode)}
	}
	return nil
}
