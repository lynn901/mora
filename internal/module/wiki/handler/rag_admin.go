package handler

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/rag"
	"github.com/wiki/wiki-backend/internal/module/wiki/service"
	"github.com/wiki/wiki-backend/internal/pkg/response"
	pkgerr "github.com/wiki/wiki-backend/internal/pkg/errors"
)

// IndexStatusHandler exposes GET /documents/:id/index-status (API 04 §9.1).
// It reuses DocumentService for the RBAC read check so existence never leaks
// (forbidden and not-found both surface as 404, PRD F1.5).
type IndexStatusHandler struct {
	status rag.IndexStatusStore
	docs   *service.DocumentService
}

func NewIndexStatusHandler(status rag.IndexStatusStore, docs *service.DocumentService) *IndexStatusHandler {
	return &IndexStatusHandler{status: status, docs: docs}
}

func (h *IndexStatusHandler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	auth := svcAuth(MustAuth(c))
	// RBAC read check: Get maps both missing and forbidden to NotFound (non-leak).
	if _, err := h.docs.Get(c.Request.Context(), auth, id); err != nil {
		response.Fail(c, err)
		return
	}
	info, err := h.status.GetDocumentIndexStatus(c.Request.Context(), id.String())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"index_status":    info.IndexStatus,
		"last_indexed_at": info.LastIndexedAt,
		"chunk_count":     info.ChunkCount,
		"error":           info.LastError,
	})
}

// EmbeddingModelHandler exposes the embedding-model admin routes (API 04 §9.2):
// GET/POST /admin/embedding-models, POST /admin/embedding-models/:id/test,
// POST /admin/embedding-models/:id/rebuild. Admin-only.
type EmbeddingModelHandler struct {
	models  rag.ModelStore
	factory rag.ProviderFactory
	events  service.EventPublisher
}

func NewEmbeddingModelHandler(models rag.ModelStore, factory rag.ProviderFactory, events service.EventPublisher) *EmbeddingModelHandler {
	return &EmbeddingModelHandler{models: models, factory: factory, events: events}
}

func requireAdmin(c *gin.Context) bool {
	if !MustAuth(c).IsAdmin {
		response.Fail(c, pkgerr.Forbidden("admin only"))
		return false
	}
	return true
}

func (h *EmbeddingModelHandler) List(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	ms, err := h.models.List(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": ms})
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

func (h *EmbeddingModelHandler) Upsert(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req upsertModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	if req.Provider == "" || req.ModelName == "" || req.Dimension <= 0 {
		response.Fail(c, badRequestErr("provider, model_name, dimension required"))
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
	saved, err := h.models.Upsert(c.Request.Context(), m)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if req.Active {
		_ = h.models.SetActive(c.Request.Context(), saved.ID)
		saved.Status = "active"
	}
	response.Created(c, saved)
}

func (h *EmbeddingModelHandler) Test(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	id := c.Param("id")
	m, err := h.models.GetByID(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, pkgerr.NotFound("model not found"))
		return
	}
	prov, err := h.factory.For(c.Request.Context(), m)
	if err != nil {
		response.OK(c, gin.H{"ok": false, "error": err.Error()})
		return
	}
	start := time.Now()
	if err := prov.HealthCheck(c.Request.Context()); err != nil {
		response.OK(c, gin.H{"ok": false, "error": err.Error()})
		return
	}
	vecs, err := prov.Embed(c.Request.Context(), []string{"ping"}, m.InstructionQuery)
	latency := time.Since(start).Milliseconds()
	if err != nil || len(vecs) != 1 {
		response.OK(c, gin.H{"ok": false, "error": "embed failed"})
		return
	}
	response.OK(c, gin.H{"ok": true, "latency_ms": latency, "dimension": len(vecs[0])})
}

func (h *EmbeddingModelHandler) Rebuild(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var body struct {
		WorkspaceID string `json:"workspace_id"`
	}
	_ = c.ShouldBindJSON(&body)
	if err := h.events.PublishModelRebuild(c.Request.Context(), body.WorkspaceID); err != nil {
		response.Fail(c, err)
		return
	}
	response.Accepted(c, gin.H{"status": "rebuild started"})
}
