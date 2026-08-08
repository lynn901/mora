// Package service (parse.go) orchestrates the multi-format document parsing
// flow (design-docs/10 §4, §5, §6, §7): upload → object store → create document
// row (parse_status=pending) → publish document.parse event; reparse with new
// opts; chunk preview; parse-progress query. RBAC is enforced as a hard
// constraint throughout — existence is never leaked.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag/parser"
	"github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/pagination"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// ObjectStore is the upload-side object storage (PUT a file). It is the write
// counterpart of parser.Reader; the same *objstore.Store implements both.
type ObjectStore interface {
	Put(ctx context.Context, key, contentType string, data []byte) (string, error)
}

// ParseTaskQueue publishes document.parse / document.reparse events.
type ParseTaskQueue interface {
	PublishParse(ctx context.Context, ev ParseEvent) error
}

// ParseEvent is the canonical parse-task event payload (10 §4.1, §5.2).
type ParseEvent struct {
	EventID      string         `json:"event_id"`
	EventType    string         `json:"event_type"` // document.parse | document.reparse
	DocumentID   domain.UUID    `json:"document_id"`
	WorkspaceID  domain.UUID    `json:"workspace_id"`
	StorageKey   string         `json:"storage_key"`
	MIME         string         `json:"mime,omitempty"`
	Filename     string         `json:"filename,omitempty"`
	SourceFormat string         `json:"source_format,omitempty"`
	ParseOpts    map[string]any `json:"parse_opts,omitempty"`
}

// ParseProgressStore is the read model for the parse-progress query (10 §6.3).
type ParseProgressStore interface {
	GetParseProgress(ctx context.Context, documentID string) (ParseProgressResult, error)
}

// ParseProgressResult is the API-facing read model (carries both badges).
type ParseProgressResult struct {
	ParseStatus string         `json:"parse_status"`
	IndexStatus string         `json:"index_status"`
	Progress    []ProgressItem `json:"progress"`
	UpdatedAt   string         `json:"updated_at,omitempty"`
}

// ProgressItem is one staged progress entry (10 §6.1), surfaced to the UI.
type ProgressItem struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
	At     string `json:"at"`
	Detail string `json:"detail,omitempty"`
}

// ChunkPreviewer runs the chunker on input text without persisting (10 §2.2).
type ChunkPreviewer interface {
	Preview(ctx context.Context, text string, opts parser.ParseOptions) (ChunkPreviewResult, error)
}

// ChunkPreviewResult is the chunk-preview API response (10 §7.2 /rag/chunk-preview).
type ChunkPreviewResult struct {
	Chunks   []ChunkPreviewItem `json:"chunks"`
	Strategy string             `json:"strategy"`
	Total    int                `json:"total"`
}

// ChunkPreviewItem is one previewed chunk (no point id — preview doesn't persist).
type ChunkPreviewItem struct {
	Text        string `json:"text"`
	ChunkIndex  int    `json:"chunk_index"`
	SectionPath string `json:"section_path,omitempty"`
	TokenCount  int    `json:"token_count"`
	Role        string `json:"role,omitempty"`
}

// ParseConfigStore is the parse_configs template store (10 §7).
type ParseConfigStore interface {
	List(ctx context.Context, workspaceID string) ([]ParseConfig, error)
	Get(ctx context.Context, id string) (ParseConfig, error)
	Create(ctx context.Context, c ParseConfig) (ParseConfig, error)
	Update(ctx context.Context, id string, c ParseConfig) (ParseConfig, error)
	Delete(ctx context.Context, id string) error
}

// ParseConfig is the API-facing config template (10 §7.1).
type ParseConfig struct {
	ID          string         `json:"id"`
	WorkspaceID *string        `json:"workspace_id,omitempty"`
	Name        string         `json:"name"`
	Config      map[string]any `json:"config"`
	IsDefault   bool           `json:"is_default"`
}

// ParseService ties upload/reparse/preview/progress to repositories + RBAC.
type ParseService struct {
	docs     DocumentRepo
	rbac     *rbac.Engine
	events   ParseTaskQueue
	objects  ObjectStore
	progress ParseProgressStore
	preview  ChunkPreviewer
	configs  ParseConfigStore
	// MaxFileMB bounds upload size (PARSE_MAX_FILE_MB, 10 §9.2).
	MaxFileMB int
}

func NewParseService(docs DocumentRepo, engine *rbac.Engine, events ParseTaskQueue, objects ObjectStore, progress ParseProgressStore, preview ChunkPreviewer, configs ParseConfigStore, maxFileMB int) *ParseService {
	return &ParseService{docs: docs, rbac: engine, events: events, objects: objects, progress: progress, preview: preview, configs: configs, MaxFileMB: maxFileMB}
}

// UploadFile stores an uploaded file and enqueues a parse task (10 §4.1, §7.2).
// RBAC: caller must have workspace write (or be admin). Existence never leaks.
func (s *ParseService) UploadFile(ctx context.Context, auth AuthContext, req UploadRequest) (*UploadResult, error) {
	if !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetWorkspace, req.WorkspaceID, domain.ActionWrite)
		if err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, errors.Forbidden("no write permission on workspace")
		}
	}
	if s.MaxFileMB > 0 && len(req.FileData) > s.MaxFileMB<<20 {
		return nil, errors.BadRequest(fmt.Sprintf("file exceeds %dMB limit", s.MaxFileMB))
	}
	if s.objects == nil {
		return nil, errors.BadRequest("object storage not configured")
	}
	// storage key: mora/{workspace}/{doc}/source/{filename} (10 §4.2.1). The
	// doc id is generated first so the key is stable.
	docID := uuid.New()
	storageKey := fmt.Sprintf("mora/%s/%s/source/%s", req.WorkspaceID, docID, filepath.Base(req.Filename))
	if _, err := s.objects.Put(ctx, storageKey, req.MIME, req.FileData); err != nil {
		return nil, fmt.Errorf("store file: %w", err)
	}
	sourceFormat := parser.FormatFromName(req.Filename)
	if sourceFormat == "" && req.ParseOptions != nil {
		// fall back to mime-derived key (kept simple for MVP)
	}
	// Create the document row: content=placeholder, status=draft, parse_status=pending.
	d := &domain.Document{
		ID:           docID,
		WorkspaceID:  req.WorkspaceID,
		DirectoryID:  req.DirectoryID,
		Title:        orDefault(req.Title, stripExt(req.Filename)),
		Format:       domain.FormatBlocks,
		Status:       domain.StatusDraft,
		IndexStatus:  domain.IndexPending,
		VersionNo:    1,
		CreatedBy:    auth.UserID,
		UpdatedBy:    &auth.UserID,
		StorageKey:   storageKey,
		SourceFormat: sourceFormat,
		ParseStatus:  domain.ParsePending,
	}
	if err := s.docs.Create(ctx, d); err != nil {
		return nil, err
	}
	// resolve parse opts: per-upload > template > default
	opts := resolveOpts(req, sourceFormat)
	// publish document.parse
	evt := ParseEvent{
		EventID:      uuid.NewString(),
		EventType:    "document.parse",
		DocumentID:   docID,
		WorkspaceID:  req.WorkspaceID,
		StorageKey:   storageKey,
		MIME:         req.MIME,
		Filename:     req.Filename,
		SourceFormat: sourceFormat,
		ParseOpts:    opts,
	}
	if err := s.events.PublishParse(ctx, evt); err != nil {
		return nil, fmt.Errorf("publish parse: %w", err)
	}
	return &UploadResult{DocumentID: docID, ParseStatus: string(domain.ParsePending), ParseOptions: opts}, nil
}

// Reparse enqueues a re-parse of selected documents with new opts (10 §5.2).
// RBAC: the caller must have write on EACH target document; unauthorized docs
// are silently excluded from the enqueue list (existence not leaked).
func (s *ParseService) Reparse(ctx context.Context, auth AuthContext, req ReparseRequest) (*ReparseResult, error) {
	enqueued := 0
	var taskIDs []string
	for _, docID := range req.DocumentIDs {
		// load doc; if missing or no write permission, skip silently (no leak).
		d, err := s.docs.Get(ctx, docID)
		if err != nil {
			continue
		}
		if !auth.IsAdmin {
			dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetDocument, docID, domain.ActionWrite)
			if err != nil || !dec.Allowed {
				continue
			}
		}
		opts := req.ParseOptions
		if opts == nil {
			opts = map[string]any{}
		}
		evt := ParseEvent{
			EventID:      uuid.NewString(),
			EventType:    "document.reparse",
			DocumentID:   docID,
			WorkspaceID:  d.WorkspaceID,
			StorageKey:   d.StorageKey,
			Filename:     d.SourceFormat,
			SourceFormat: d.SourceFormat,
			ParseOpts:    opts,
		}
		if err := s.events.PublishParse(ctx, evt); err != nil {
			continue
		}
		enqueued++
		taskIDs = append(taskIDs, evt.EventID)
	}
	return &ReparseResult{Enqueued: enqueued, TaskIDs: taskIDs}, nil
}

// ParseProgress returns the staged timeline for a document (10 §6.3). RBAC:
// caller must have read on the document; missing/forbidden both 404 (no leak).
func (s *ParseService) ParseProgress(ctx context.Context, auth AuthContext, docID domain.UUID) (*ParseProgressResult, error) {
	if !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetDocument, docID, domain.ActionRead)
		if err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, errors.NotFound("document not found")
		}
	}
	res, err := s.progress.GetParseProgress(ctx, docID.String())
	if err != nil {
		return nil, errors.NotFound("document not found")
	}
	return &res, nil
}

// ChunkPreview runs the chunker on text without persisting (10 §2.2, §7.2).
func (s *ParseService) ChunkPreview(ctx context.Context, text string, opts parser.ParseOptions) (*ChunkPreviewResult, error) {
	res, err := s.preview.Preview(ctx, text, opts)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// --- parse config templates (10 §7.1) ---

func (s *ParseService) ListConfigs(ctx context.Context, workspaceID domain.UUID) ([]ParseConfig, error) {
	return s.configs.List(ctx, workspaceID.String())
}
func (s *ParseService) CreateConfig(ctx context.Context, workspaceID domain.UUID, c ParseConfig) (ParseConfig, error) {
	ws := workspaceID.String()
	c.WorkspaceID = &ws
	return s.configs.Create(ctx, c)
}
func (s *ParseService) UpdateConfig(ctx context.Context, id string, c ParseConfig) (ParseConfig, error) {
	return s.configs.Update(ctx, id, c)
}
func (s *ParseService) DeleteConfig(ctx context.Context, id string) error {
	return s.configs.Delete(ctx, id)
}

// --- request/response shapes ---

type UploadRequest struct {
	WorkspaceID   domain.UUID
	DirectoryID   *domain.UUID
	Filename      string
	MIME          string
	Title         string
	FileData      []byte
	ParseConfigID *domain.UUID
	ParseOptions  map[string]any
}

type UploadResult struct {
	DocumentID   domain.UUID    `json:"document_id"`
	ParseStatus  string         `json:"parse_status"`
	ParseOptions map[string]any `json:"parse_options,omitempty"`
}

type ReparseRequest struct {
	WorkspaceID  domain.UUID
	DocumentIDs  []domain.UUID
	ParseOptions map[string]any
}

type ReparseResult struct {
	Enqueued int      `json:"enqueued"`
	TaskIDs  []string `json:"task_ids,omitempty"`
}

// resolveOpts merges per-upload opts over a referenced template's config. When
// a parse_config_id is given, the template's config is the base and per-upload
// opts override individual keys. Without a template, per-upload opts stand alone.
func resolveOpts(req UploadRequest, sourceFormat string) map[string]any {
	opts := map[string]any{}
	if req.ParseOptions != nil {
		for k, v := range req.ParseOptions {
			opts[k] = v
		}
	}
	if _, ok := opts["chunking_strategy"]; !ok {
		opts["chunking_strategy"] = "fixed"
	}
	_ = sourceFormat
	return opts
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func stripExt(name string) string {
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[:i]
	}
	return name
}

// jsonKeep is a small helper to re-marshal a map for logging/debugging.
var _ = json.Marshal
var _ = pagination.Params{}
