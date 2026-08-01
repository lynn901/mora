// Package qdrant implements rag.VectorStore against the Qdrant REST API
// (03-data-model.md §3 / 05 §4). It is the production vector backend; the
// pipeline and search engine talk to it through the rag.VectorStore interface.
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag"
)

// Client is a thin Qdrant REST client implementing rag.VectorStore.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) EnsureCollection(ctx context.Context, name string, dim int) error {
	body := map[string]any{
		"vectors":         map[string]any{"size": dim, "distance": "Cosine"},
		"hnsw_config":     map[string]any{"m": 16, "ef_construct": 100, "full_scan_threshold": 10000},
		"on_disk_payload": false,
	}
	// PUT is idempotent: creates or no-ops if exists with same config.
	if _, err := c.do(ctx, http.MethodPut, "/collections/"+name, body); err != nil {
		// Qdrant returns 409 if collection exists with different config; treat existing as ok.
		if !isConflict(err) {
			return err
		}
	}
	// payload indexes (accelerate RBAC filtering)
	for _, field := range []string{"workspace_id", "status", "document_id", "visible_to", "tags"} {
		_ = c.createIndex(ctx, name, field)
	}
	return nil
}

func (c *Client) createIndex(ctx context.Context, coll, field string) error {
	_, err := c.do(ctx, http.MethodPut, "/collections/"+coll+"/index?wait=true",
		map[string]any{"field_name": field, "field_schema": "keyword"})
	return err
}

func (c *Client) UpsertChunks(ctx context.Context, coll string, points []rag.VectorPoint) error {
	if len(points) == 0 {
		return nil
	}
	qp := make([]map[string]any, len(points))
	for i, p := range points {
		qp[i] = map[string]any{"id": p.PointID, "vector": p.Vector, "payload": p.Payload}
	}
	_, err := c.do(ctx, http.MethodPut, "/collections/"+coll+"/points?wait=true",
		map[string]any{"points": qp})
	return err
}

func (c *Client) DeleteByDocument(ctx context.Context, coll, docID string) error {
	return c.deleteByFilter(ctx, coll, must("document_id", docID))
}

func (c *Client) DeleteByDocumentVersion(ctx context.Context, coll, docID string, versionNo int) error {
	return c.deleteByFilter(ctx, coll, must("document_id", docID), mustMatchInt("version_no", versionNo))
}

func (c *Client) deleteByFilter(ctx context.Context, coll string, mustConds ...map[string]any) error {
	_, err := c.do(ctx, http.MethodPost, "/collections/"+coll+"/points/delete?wait=true",
		map[string]any{"filter": map[string]any{"must": mustConds}})
	return err
}

func (c *Client) SetVisibleTo(ctx context.Context, coll, docID string, vis []string) error {
	_, err := c.do(ctx, http.MethodPost, "/collections/"+coll+"/points/payload?wait=true",
		map[string]any{
			"payload": map[string]any{"visible_to": vis},
			"filter":  map[string]any{"must": []map[string]any{must("document_id", docID)}},
		})
	return err
}

func (c *Client) SearchDense(ctx context.Context, req rag.VectorSearchRequest) ([]rag.VectorHit, error) {
	mustConds := []map[string]any{must("status", string(domain.DocPublished))}
	if req.WorkspaceID != "" {
		mustConds = append(mustConds, must("workspace_id", req.WorkspaceID))
	}
	if req.DirectoryID != "" {
		mustConds = append(mustConds, must("directory_id", req.DirectoryID))
	}
	// RBAC HARD FILTER: visible_to must intersect the viewer's subjects.
	if len(req.VisibleTo) > 0 {
		mustConds = append(mustConds, map[string]any{
			"key": "visible_to", "match": map[string]any{"any": req.VisibleTo},
		})
	}
	if len(req.Tags) > 0 {
		mustConds = append(mustConds, map[string]any{
			"key": "tags", "match": map[string]any{"any": req.Tags},
		})
	}
	body := map[string]any{
		"vector":       req.Vector,
		"filter":       map[string]any{"must": mustConds},
		"limit":        req.TopK,
		"with_payload": true,
	}
	raw, err := c.do(ctx, http.MethodPost, "/collections/"+req.CollectionName+"/points/search", body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result []struct {
			ID      string         `json:"id"`
			Score   float32        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("qdrant search decode: %w", err)
	}
	out := make([]rag.VectorHit, 0, len(resp.Result))
	for _, r := range resp.Result {
		out = append(out, rag.VectorHit{PointID: r.ID, Score: r.Score, Payload: payloadToMeta(r.Payload)})
	}
	return out, nil
}

// --- helpers ---

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return raw, &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	return raw, nil
}

// APIError carries a Qdrant error response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("qdrant %d: %s", e.Status, snippet(e.Body)) }

func isConflict(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == http.StatusConflict
	}
	return false
}

func must(key, value string) map[string]any {
	return map[string]any{"key": key, "match": map[string]any{"value": value}}
}

func mustMatchInt(key string, value int) map[string]any {
	return map[string]any{"key": key, "match": map[string]any{"value": value}}
}

func payloadToMeta(p map[string]any) domain.ChunkMetadata {
	m := domain.ChunkMetadata{
		DocumentID:  str(p["document_id"]),
		WorkspaceID: str(p["workspace_id"]),
		DirectoryID: str(p["directory_id"]),
		ChunkText:   str(p["chunk_text"]),
		SectionPath: str(p["section_path"]),
		ModelID:     str(p["model_id"]),
		Status:      str(p["status"]),
	}
	m.VersionNo = toInt(p["version_no"])
	m.ChunkIndex = toInt(p["chunk_index"])
	m.Tags = toStringSlice(p["tags"])
	m.VisibleTo = toStringSlice(p["visible_to"])
	return m
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func snippet(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
