// Package parsewiring bridges the RAG module's interfaces to the mora parse
// service's service-layer ports, so the parse handler can publish events +
// read progress + preview chunks through the same Valkey/Qdrant/pg infra the
// rag-worker uses — without the handler depending on RAG internals.
package parsewiring

import (
	"context"
	"fmt"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/objstore"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/module/rag"
	"github.com/lynn901/mora/internal/module/rag/parser"
	"github.com/lynn901/mora/internal/module/rag/pipeline"
)

// QueueAdapter publishes document.parse / document.reparse events through the
// rag.EventQueue (the same Valkey stream the rag-worker consumes), so the
// producer and consumer share one stream format + event vocabulary.
type QueueAdapter struct {
	Queue rag.EventQueue
}

func (a QueueAdapter) PublishParse(ctx context.Context, ev service.ParseEvent) error {
	opts := ev.ParseOpts
	if opts == nil {
		opts = map[string]any{}
	}
	_, err := a.Queue.Publish(ctx, domain.DocEvent{
		EventID:     ev.EventID,
		EventType:   domain.EventType(ev.EventType),
		DocumentID:  ev.DocumentID.String(),
		WorkspaceID: ev.WorkspaceID.String(),
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Payload: map[string]any{
			"storage_key":   ev.StorageKey,
			"mime":          ev.MIME,
			"filename":      ev.Filename,
			"source_format": ev.SourceFormat,
			"parse_opts":    opts,
		},
	})
	return err
}

// ProgressAdapter reads parse progress through rag.ParseTaskStore (the pg impl).
type ProgressAdapter struct {
	Store rag.ParseTaskStore
}

func (a ProgressAdapter) GetParseProgress(ctx context.Context, documentID string) (service.ParseProgressResult, error) {
	info, err := a.Store.GetParseProgress(ctx, documentID)
	if err != nil {
		return service.ParseProgressResult{}, err
	}
	items := make([]service.ProgressItem, 0, len(info.Progress))
	for _, s := range info.Progress {
		items = append(items, service.ProgressItem{
			Stage: s.Stage, Status: s.Status, At: s.At, Detail: s.Detail,
		})
	}
	updated := ""
	if info.UpdatedAt != nil {
		updated = info.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return service.ParseProgressResult{
		ParseStatus: string(info.ParseStatus),
		IndexStatus: string(info.IndexStatus),
		Progress:    items,
		UpdatedAt:   updated,
	}, nil
}

// ObjectStoreAdapter exposes objstore.Store as service.ObjectStore (the upload
// Put path). Read is served by the same store (it implements parser.Reader).
type ObjectStoreAdapter struct {
	Store *objstore.Store
}

func (a ObjectStoreAdapter) Put(ctx context.Context, key, contentType string, data []byte) (string, error) {
	if a.Store == nil {
		return "", fmt.Errorf("object storage not configured")
	}
	return a.Store.Put(ctx, key, contentType, data)
}

// ChunkPreviewerImpl runs the pipeline chunker on input text (10 §2.2). It is
// the chunk-preview API backing: no persist, no vectorize, just the refs.
type ChunkPreviewerImpl struct {
	Tok pipeline.Tokenizer
}

func (c ChunkPreviewerImpl) Preview(ctx context.Context, text string, opts parser.ParseOptions) (service.ChunkPreviewResult, error) {
	cfg := pipeline.Config{
		ChunkSize:              orInt(opts.ChunkSize, 512),
		ChunkOverlap:           orInt(opts.ChunkOverlap, 64),
		MaxChunkSize:           1024,
		Strategy:               pipeline.Strategy(orStr(opts.ChunkingStrategy, string(pipeline.StrategyFixed))),
		RespectHeadingBoundary: true,
	}
	tok := c.Tok
	if tok == nil {
		tok = pipeline.NewWordTokenizer()
	}
	refs := pipeline.Chunk(text, cfg, tok)
	items := make([]service.ChunkPreviewItem, 0, len(refs))
	for _, r := range refs {
		items = append(items, service.ChunkPreviewItem{
			Text: r.Text, ChunkIndex: r.ChunkIndex, SectionPath: r.SectionPath,
			TokenCount: r.TokenCount, Role: string(r.Role),
		})
	}
	return service.ChunkPreviewResult{
		Chunks: items, Strategy: string(cfg.Strategy), Total: len(items),
	}, nil
}

// ParseConfigStoreAdapter exposes the pg.ParseConfigStore as the service port.
// It is a thin pass-through; the concrete store is injected at wiring time so
// the handler/service never import infra/pg directly (avoids a layering cycle).
type ParseConfigStoreAdapter struct {
	Lister  func(ctx context.Context, workspaceID string) ([]service.ParseConfig, error)
	Creater func(ctx context.Context, c service.ParseConfig) (service.ParseConfig, error)
	Updater func(ctx context.Context, id string, c service.ParseConfig) (service.ParseConfig, error)
	Deleter func(ctx context.Context, id string) error
}

func (a ParseConfigStoreAdapter) List(ctx context.Context, workspaceID string) ([]service.ParseConfig, error) {
	return a.Lister(ctx, workspaceID)
}
func (a ParseConfigStoreAdapter) Create(ctx context.Context, c service.ParseConfig) (service.ParseConfig, error) {
	return a.Creater(ctx, c)
}
func (a ParseConfigStoreAdapter) Update(ctx context.Context, id string, c service.ParseConfig) (service.ParseConfig, error) {
	return a.Updater(ctx, id, c)
}
func (a ParseConfigStoreAdapter) Delete(ctx context.Context, id string) error {
	return a.Deleter(ctx, id)
}

func orInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
func orStr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
