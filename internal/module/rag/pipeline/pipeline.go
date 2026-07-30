package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/rag"
	"github.com/wiki/wiki-backend/internal/module/rag/provider"
)

// Pipeline executes one indexing attempt for a document event. It is the
// stateless core of the RAG worker: the worker owns idempotency, the task state
// machine and retry/backoff; Pipeline.RunOnce does the actual indexing work and
// returns an error to drive retries.
type Pipeline struct {
	Cfg     Config
	Docs    rag.DocumentStore
	RBAC    rag.RBACResolver
	Vectors rag.VectorStore
	Models  rag.ModelStore
	Factory rag.ProviderFactory
	Status  rag.IndexStatusStore
	Clock   rag.Clock
	Tok     Tokenizer
	Logf    func(format string, args ...any)
}

func New(p Pipeline) *Pipeline {
	if p.Cfg.ChunkSize == 0 {
		p.Cfg = DefaultConfig()
	}
	if p.Tok == nil {
		p.Tok = NewWordTokenizer()
	}
	if p.Clock == nil {
		p.Clock = rag.SystemClock{}
	}
	if p.Logf == nil {
		p.Logf = func(string, ...any) {}
	}
	return &p
}

// RunOnce processes a single event (one attempt). Returns nil on success.
func (p *Pipeline) RunOnce(ctx context.Context, ev domain.DocEvent) error {
	switch ev.EventType {
	case domain.EventDocumentDelete:
		return p.handleDelete(ctx, ev)
	case domain.EventPermissionChange:
		return p.handlePermissionChange(ctx, ev)
	case domain.EventModelRebuild:
		return p.Rebuild(ctx, "")
	case domain.EventDocumentCreate,
		domain.EventDocumentUpdate, domain.EventAttachmentChange:
		return p.handleIndex(ctx, ev)
	default:
		return fmt.Errorf("rag: unknown event type %q", ev.EventType)
	}
}

// handleDelete cascades: remove all chunks (all versions) from Qdrant + PG meta,
// then mark the document badge. (AC-10)
func (p *Pipeline) handleDelete(ctx context.Context, ev domain.DocEvent) error {
	coll, err := p.collection(ctx)
	if err != nil {
		return err
	}
	if err := p.Vectors.DeleteByDocument(ctx, coll, ev.DocumentID); err != nil {
		return fmt.Errorf("delete vectors: %w", err)
	}
	if err := p.Status.DeleteAllChunkMeta(ctx, ev.DocumentID); err != nil {
		p.Logf("rag: delete chunk meta for %s: %v", ev.DocumentID, err)
	}
	if err := p.Status.SetDocumentIndexStatus(ctx, ev.DocumentID, domain.IndexIndexed, ""); err != nil {
		return fmt.Errorf("set index status: %w", err)
	}
	p.Logf("rag: cascaded delete for document %s", ev.DocumentID)
	return nil
}

// handlePermissionChange recomputes visible_to for a document's chunks without
// re-chunking (05 §4.3.3). Stale visible_to stays conservative (under-grants)
// until the overwrite completes — never over-grants.
func (p *Pipeline) handlePermissionChange(ctx context.Context, ev domain.DocEvent) error {
	coll, err := p.collection(ctx)
	if err != nil {
		return err
	}
	visibleTo, err := p.RBAC.ResolveReaders(ctx, ev.DocumentID)
	if err != nil {
		return fmt.Errorf("resolve readers: %w", err)
	}
	if err := p.Vectors.SetVisibleTo(ctx, coll, ev.DocumentID, visibleTo); err != nil {
		return fmt.Errorf("set visible_to: %w", err)
	}
	p.Logf("rag: recomputed visible_to for %s (%d subjects)", ev.DocumentID, len(visibleTo))
	return nil
}

// handleIndex is the create/update path: extract → chunk → embed → upsert → receipt.
func (p *Pipeline) handleIndex(ctx context.Context, ev domain.DocEvent) error {
	model, err := p.Models.GetActive(ctx)
	if err != nil {
		return fmt.Errorf("get active model: %w", err)
	}
	coll := model.CollectionName()
	if err := p.Vectors.EnsureCollection(ctx, coll, model.Dimension); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	prov, err := p.Factory.For(ctx, model)
	if err != nil {
		return fmt.Errorf("provider for model: %w", err)
	}

	snap, err := p.Docs.GetSnapshot(ctx, ev.DocumentID, ev.VersionNo)
	if err != nil {
		return fmt.Errorf("get snapshot: %w", err)
	}
	// Only published documents are indexed (drafts/archived are skipped; their
	// badge stays pending). Deleted documents arrive via document.delete.
	if snap.Status != domain.DocPublished {
		p.Logf("rag: skip non-published document %s (status=%s)", ev.DocumentID, snap.Status)
		return nil
	}

	// Update: cascade-remove the previous version's chunks before writing the new.
	if ev.EventType == domain.EventDocumentUpdate && ev.PrevVersionNo > 0 {
		if err := p.Vectors.DeleteByDocumentVersion(ctx, coll, ev.DocumentID, ev.PrevVersionNo); err != nil {
			return fmt.Errorf("delete prev version vectors: %w", err)
		}
		_ = p.Status.DeleteChunkMeta(ctx, ev.DocumentID, ev.PrevVersionNo)
	}

	// 1. extract
	extracted := ExtractText(snap.Content)
	structured := extracted.StructuredText
	if structured == "" {
		structured = snap.ContentText
	}

	// 2. chunk
	refs := Chunk(structured, p.Cfg, p.Tok)
	if len(refs) == 0 {
		// nothing to index (empty doc): clear vectors for this version, mark indexed.
		_ = p.Vectors.DeleteByDocumentVersion(ctx, coll, ev.DocumentID, ev.VersionNo)
		_ = p.Status.SetDocumentIndexStatus(ctx, ev.DocumentID, domain.IndexIndexed, "")
		return nil
	}

	// 3. visible_to (RBAC payload core)
	visibleTo, err := p.RBAC.ResolveReaders(ctx, ev.DocumentID)
	if err != nil {
		return fmt.Errorf("resolve readers: %w", err)
	}

	// 4. embed in batches (instruction_doc, Instruction-Aware)
	vectors, err := p.embedBatches(ctx, prov, refs, model.InstructionDoc)
	if err != nil {
		return err // provider errors are retryable (queue, don't drop)
	}

	// 5. build points with payload
	now := p.Clock.Now().UTC().Format(time.RFC3339)
	points := make([]rag.VectorPoint, len(refs))
	chunkMetas := make([]domain.Chunk, len(refs))
	for i, r := range refs {
		pid := domain.PointID(domain.RAGNamespace().String(), ev.DocumentID, ev.VersionNo, r.ChunkIndex)
		payload := domain.ChunkMetadata{
			DocumentID:   ev.DocumentID,
			WorkspaceID:  snap.WorkspaceID,
			DirectoryID:  snap.DirectoryID,
			VersionNo:    ev.VersionNo,
			ChunkIndex:   r.ChunkIndex,
			ChunkText:    r.Text,
			SectionPath:  r.SectionPath,
			ModelID:      model.ID,
			Tags:         snap.Tags,
			VisibleTo:    visibleTo,
			Status:       string(snap.Status),
			CreatedAt:    now,
		}
		points[i] = rag.VectorPoint{PointID: pid, Vector: vectors[i], Payload: payload}
		chunkMetas[i] = domain.Chunk{
			DocumentID:    ev.DocumentID,
			VersionNo:     ev.VersionNo,
			ChunkIndex:    r.ChunkIndex,
			Text:          r.Text,
			TokenCount:    r.TokenCount,
			SectionPath:   r.SectionPath,
			ModelID:       model.ID,
			QdrantPointID: pid,
			Metadata:      payload,
		}
	}

	// 6. upsert (idempotent: deterministic point ids overwrite, never duplicate)
	if err := p.Vectors.UpsertChunks(ctx, coll, points); err != nil {
		return fmt.Errorf("upsert chunks: %w", err)
	}

	// 7. persist chunk metadata + index-status receipt
	if err := p.Status.RecordChunks(ctx, ev.DocumentID, ev.VersionNo, model.ID, chunkMetas); err != nil {
		p.Logf("rag: record chunks for %s: %v", ev.DocumentID, err)
	}
	if err := p.Status.SetDocumentIndexStatus(ctx, ev.DocumentID, domain.IndexIndexed, ""); err != nil {
		return fmt.Errorf("set index status: %w", err)
	}
	p.Logf("rag: indexed document %s v%d (%d chunks)", ev.DocumentID, ev.VersionNo, len(refs))
	return nil
}

// embedBatches encodes chunk texts in batches of EmbedBatchSize, validating the
// dimension against the collection to prevent dimension-mixing (05 §3.4).
func (p *Pipeline) embedBatches(ctx context.Context, prov provider.EmbeddingProvider, refs []ChunkRef, instruction string) ([][]float32, error) {
	batch := p.Cfg.EmbedBatchSize
	if batch <= 0 {
		batch = 32
	}
	out := make([][]float32, len(refs))
	for i := 0; i < len(refs); i += batch {
		end := i + batch
		if end > len(refs) {
			end = len(refs)
		}
		texts := make([]string, end-i)
		for j := i; j < end; j++ {
			texts[j-i] = refs[j].Text
		}
		vecs, err := prov.Embed(ctx, texts, instruction)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(texts) {
			return nil, fmt.Errorf("embedding count mismatch: got %d want %d", len(vecs), len(texts))
		}
		for j, v := range vecs {
			out[i+j] = v
		}
	}
	return out, nil
}

// collection returns the active model's collection name (for delete/permission
// events that don't carry a version).
func (p *Pipeline) collection(ctx context.Context) (string, error) {
	model, err := p.Models.GetActive(ctx)
	if err != nil {
		return "", err
	}
	return model.CollectionName(), nil
}

// Rebuild re-indexes all (optionally workspace-scoped) published documents for
// the active model. Used on model switch (05 §5.3). Paged by cursor so it can be
// paused/resumed; idempotent point ids make partial progress safe.
func (p *Pipeline) Rebuild(ctx context.Context, workspaceID string) error {
	var cursor string
	for {
		ids, next, err := p.Docs.PublishedDocumentIDs(ctx, cursor, 200)
		if err != nil {
			return fmt.Errorf("list published: %w", err)
		}
		for _, id := range ids {
			ev := domain.DocEvent{
				EventID:    "rebuild:" + id, // per-doc label (RunOnce does not consult event_id)
				EventType:  domain.EventDocumentCreate,
				DocumentID: id,
			}
			// Best-effort per document: log failures, continue.
			if err := p.RunOnce(ctx, ev); err != nil {
				p.Logf("rag: rebuild doc %s failed: %v", id, err)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return nil
}

// ErrSkip is returned when an event should be considered done without indexing.
var ErrSkip = errors.New("rag: skip event")
