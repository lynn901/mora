// Package handler exposes the RAG HTTP endpoints (API 04 §9): semantic hybrid
// search, document index-status, and embedding-model admin. Handlers are plain
// net/http so any router can mount them; they depend only on RAG ports and an
// Authenticator.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag"
	"github.com/lynn901/mora/internal/module/rag/pipeline"
	"github.com/lynn901/mora/internal/module/rag/search"
)

// Authenticator resolves the authenticated user id from a request.
type Authenticator interface {
	UserID(r *http.Request) (string, error)
}

// DocGuard authorizes a user to read a document (index-status endpoint). If nil,
// the handler trusts upstream authorization middleware.
type DocGuard func(ctx context.Context, userID, documentID string) (bool, error)

// Handler wires RAG endpoints.
type Handler struct {
	Search   *search.HybridSearcher
	Status   rag.IndexStatusStore
	Models   rag.ModelStore
	Factory  rag.ProviderFactory
	Pipeline *pipeline.Pipeline
	Auth     Authenticator
	Guard    DocGuard
}

// Routes returns a ServeMux with all RAG routes registered. Go 1.22 pattern
// matching is used so callers can also mount individual handlers.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/rag/search", h.handleSearch)
	mux.HandleFunc("GET /api/v1/documents/{id}/index-status", h.handleIndexStatus)
	mux.HandleFunc("GET /api/v1/admin/embedding-models", h.handleListModels)
	mux.HandleFunc("POST /api/v1/admin/embedding-models", h.handleUpsertModel)
	mux.HandleFunc("POST /api/v1/admin/embedding-models/{id}/test", h.handleTestModel)
	mux.HandleFunc("POST /api/v1/admin/embedding-models/{id}/rebuild", h.handleRebuild)
	return mux
}

// --- search ---

type searchReq struct {
	Query       string         `json:"query"`
	WorkspaceID string         `json:"workspace_id"`
	DirectoryID string         `json:"directory_id"`
	Tags        []string       `json:"tags"`
	TopK        int            `json:"top_k"`
	TopN        int            `json:"top_n"`
	Rerank      bool           `json:"rerank"`
	Filters     map[string]any `json:"filters"`
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	uid, err := h.Auth.UserID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err)
		return
	}
	var req searchReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request", err)
		return
	}
	res, err := h.Search.Search(r.Context(), search.SearchRequest{
		Query:       req.Query,
		UserID:      uid,
		WorkspaceID: req.WorkspaceID,
		DirectoryID: req.DirectoryID,
		Tags:        req.Tags,
		TopK:        req.TopK,
		TopN:        req.TopN,
		Rerank:      req.Rerank,
		Filters:     req.Filters,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "search failed", err)
		return
	}
	writeOK(w, res)
}

// --- index status ---

func (h *Handler) handleIndexStatus(w http.ResponseWriter, r *http.Request) {
	uid, err := h.Auth.UserID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err)
		return
	}
	docID := r.PathValue("id")
	if docID == "" {
		writeErr(w, http.StatusBadRequest, "missing id", nil)
		return
	}
	if h.Guard != nil {
		ok, err := h.Guard(r.Context(), uid, docID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "authz check failed", err)
			return
		}
		if !ok {
			// 404 (not 403) to avoid leaking existence (PRD F1.5).
			writeErr(w, http.StatusNotFound, "not found", nil)
			return
		}
	}
	info, err := h.Status.GetDocumentIndexStatus(r.Context(), docID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "status lookup failed", err)
		return
	}
	writeOK(w, map[string]any{
		"index_status":    info.IndexStatus,
		"last_indexed_at": info.LastIndexedAt,
		"chunk_count":     info.ChunkCount,
		"error":           info.LastError,
	})
}

// --- embedding models admin ---

func (h *Handler) handleListModels(w http.ResponseWriter, r *http.Request) {
	ms, err := h.Models.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list models failed", err)
		return
	}
	writeOK(w, ms)
}

type upsertModelReq struct {
	ID               string `json:"id"`
	Provider         string `json:"provider"`
	ModelName        string `json:"model_name"`
	Dimension        int    `json:"dimension"`
	MaxToken         int    `json:"max_token"`
	InstructionQuery string `json:"instruction_query"`
	InstructionDoc   string `json:"instruction_doc"`
	Active           bool   `json:"active"`
}

func (h *Handler) handleUpsertModel(w http.ResponseWriter, r *http.Request) {
	var req upsertModelReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request", err)
		return
	}
	if req.Provider == "" || req.ModelName == "" || req.Dimension <= 0 {
		writeErr(w, http.StatusBadRequest, "provider, model_name, dimension required", nil)
		return
	}
	m := domain.EmbeddingModel{
		ID:               req.ID,
		Provider:         req.Provider,
		ModelName:        req.ModelName,
		Dimension:        req.Dimension,
		MaxToken:         req.MaxToken,
		InstructionQuery: req.InstructionQuery,
		InstructionDoc:   req.InstructionDoc,
		Status:           "active",
	}
	saved, err := h.Models.Upsert(r.Context(), m)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "upsert model failed", err)
		return
	}
	if req.Active {
		_ = h.Models.SetActive(r.Context(), saved.ID)
		saved.Status = "active"
	}
	w.WriteHeader(http.StatusCreated)
	writeOK(w, saved)
}

func (h *Handler) handleTestModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := h.Models.GetByID(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "model not found", err)
		return
	}
	prov, err := h.Factory.For(r.Context(), m)
	if err != nil {
		writeOK(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	start := time.Now()
	if err := prov.HealthCheck(r.Context()); err != nil {
		writeOK(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	vecs, err := prov.Embed(r.Context(), []string{"ping"}, m.InstructionQuery)
	latency := time.Since(start).Milliseconds()
	if err != nil || len(vecs) != 1 {
		writeOK(w, map[string]any{"ok": false, "error": "embed failed"})
		return
	}
	writeOK(w, map[string]any{"ok": true, "latency_ms": latency, "dimension": len(vecs[0])})
}

func (h *Handler) handleRebuild(w http.ResponseWriter, r *http.Request) {
	if h.Pipeline == nil {
		writeErr(w, http.StatusServiceUnavailable, "pipeline not configured", nil)
		return
	}
	var body struct {
		WorkspaceID string `json:"workspace_id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	// Run rebuild asynchronously; idempotent point ids make partial progress safe.
	go func(ws string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := h.Pipeline.Rebuild(ctx, ws); err != nil {
			_ = err
		}
	}(body.WorkspaceID)
	w.WriteHeader(http.StatusAccepted)
	writeOK(w, map[string]any{"status": "rebuild started"})
}

// --- response helpers ---

func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
}

func writeErr(w http.ResponseWriter, status int, msg string, cause error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	detail := msg
	if cause != nil && !errors.Is(cause, context.Canceled) {
		detail = msg + ": " + cause.Error()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"code": status, "message": detail})
}

// ReadUserID is a convenience for tests/fakes that inline an Authenticator.
func ReadUserID(r *http.Request) (string, error) {
	if v := r.Header.Get("X-User-ID"); v != "" {
		return v, nil
	}
	return "", errors.New("no user id")
}

// strSplit is a tiny helper kept for future filter parsing.
func strSplit(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
