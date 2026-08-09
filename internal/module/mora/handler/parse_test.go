package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/module/rag/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// previewChunker is a minimal ChunkPreviewer for the handler test: it echoes
// the input text so the success path can assert the chunker was reached.
type previewChunker struct {
	called bool
	text   string
}

func (p *previewChunker) Preview(ctx context.Context, text string, opts parser.ParseOptions) (service.ChunkPreviewResult, error) {
	p.called = true
	p.text = text
	return service.ChunkPreviewResult{Total: 1, Strategy: "fixed", Chunks: []service.ChunkPreviewItem{{Text: text, ChunkIndex: 0}}}, nil
}

// newChunkPreviewHandler builds a ParseHandler whose ParseService carries only
// the preview stub; the other dependencies are nil because ChunkPreview never
// touches them.
func newChunkPreviewHandler(p *previewChunker) *ParseHandler {
	svc := service.NewParseService(nil, nil, nil, nil, nil, p, nil, 0)
	return NewParseHandler(svc)
}

func TestChunkPreview_MissingTextReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chunker := &previewChunker{}
	h := newChunkPreviewHandler(chunker)
	r := gin.New()
	r.POST("/rag/chunk-preview", h.ChunkPreview)

	body := `{"parse_options":{}}`
	req := httptest.NewRequest(http.MethodPost, "/rag/chunk-preview", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "missing required text must be 400, not 200")
	var env struct {
		Code    int    `json:"code"`
		Data    any    `json:"data"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.NotEqual(t, 0, env.Code, "error envelope code must be non-zero")
	assert.Contains(t, env.Message, "text")
	assert.False(t, chunker.called, "chunker must not run when text is missing")
}

func TestChunkPreview_EmptyTextReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chunker := &previewChunker{}
	h := newChunkPreviewHandler(chunker)
	r := gin.New()
	r.POST("/rag/chunk-preview", h.ChunkPreview)

	body := `{"text":"   ","parse_options":{}}`
	req := httptest.NewRequest(http.MethodPost, "/rag/chunk-preview", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "whitespace-only text must be treated as missing")
	assert.False(t, chunker.called, "chunker must not run on empty text")
}

func TestChunkPreview_NonEmptyTextReturns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chunker := &previewChunker{}
	h := newChunkPreviewHandler(chunker)
	r := gin.New()
	r.POST("/rag/chunk-preview", h.ChunkPreview)

	body := `{"text":"sample text","parse_options":{}}`
	req := httptest.NewRequest(http.MethodPost, "/rag/chunk-preview", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var env struct {
		Code int `json:"code"`
		Data struct {
			Chunks   []service.ChunkPreviewItem `json:"chunks"`
			Strategy string                      `json:"strategy"`
			Total    int                         `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, 0, env.Code)
	assert.Equal(t, 1, env.Data.Total)
	assert.True(t, chunker.called, "chunker must run for non-empty text")
	assert.Equal(t, "sample text", chunker.text)
}
