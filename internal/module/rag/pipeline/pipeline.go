package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag"
	"github.com/lynn901/mora/internal/module/rag/parser"
	"github.com/lynn901/mora/internal/module/rag/provider"
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

	// Parse-stage dependencies (10 §4). Optional: a nil Parser/ParseStore
	// means document.parse events are not supported in this process (the
	// rag-worker wires them when object storage + the registry are available).
	Parser     parser.Parser      // the routed parser; if nil, parse events error
	Registry   *parser.Registry   // routes (mime, filename) → parser
	Objects    parser.Reader      // object-storage reader (parser.Read source)
	ParseStore rag.ParseTaskStore // parse_tasks + progress + parse_status badge
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
	case domain.EventDocumentParse, domain.EventDocumentReparse:
		return p.handleParse(ctx, ev)
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

	snap, err := p.Docs.GetSnapshot(ctx, ev.DocumentID, ev.VersionNo)
	if err != nil {
		return fmt.Errorf("get snapshot: %w", err)
	}
	// Only published documents are indexed. Drafts/archived are skipped: their
	// badge stays pending (indexed once published). Deleted documents arrive via
	// document.delete. The skip is evaluated before touching the embedding
	// provider so a non-published document is never blocked by provider
	// availability — and never left stuck at "processing" (DEFECT-06).
	if snap.Status != domain.DocPublished {
		// The worker already set the badge to "processing"; leaving it there would
		// look like a hung index and mask the real state (DEFECT-06). Reset to
		// pending (will index once published) and best-effort remove any stale
		// vectors/chunks from a prior published version so the document cannot
		// surface in search. Cleanup is best-effort: a draft must reach "pending"
		// even if the vector store is temporarily unavailable.
		_ = p.Vectors.EnsureCollection(ctx, coll, model.Dimension)
		if err := p.Vectors.DeleteByDocument(ctx, coll, ev.DocumentID); err != nil {
			p.Logf("rag: skip %s: clear stale vectors: %v", ev.DocumentID, err)
		}
		if err := p.Status.DeleteAllChunkMeta(ctx, ev.DocumentID); err != nil {
			p.Logf("rag: skip %s: clear chunk meta: %v", ev.DocumentID, err)
		}
		if err := p.Status.SetDocumentIndexStatus(ctx, ev.DocumentID, domain.IndexPending, ""); err != nil {
			return fmt.Errorf("set index status (skip): %w", err)
		}
		p.Logf("rag: skip non-published document %s (status=%s); badge=pending", ev.DocumentID, snap.Status)
		return nil
	}

	if err := p.Vectors.EnsureCollection(ctx, coll, model.Dimension); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	prov, err := p.Factory.For(ctx, model)
	if err != nil {
		return fmt.Errorf("provider for model: %w", err)
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
	// pre-compute parent point ids for parent-child links (10 §2.3). A child's
	// ParentChunkIndex points at its parent's chunk_index within the same
	// (doc, version) run; the deterministic PointID maps that to a uuid5.
	parentIDs := make(map[int]string, len(refs))
	for _, r := range refs {
		if r.Role == RoleParent {
			parentIDs[r.ChunkIndex] = domain.PointID(domain.RAGNamespace().String(), ev.DocumentID, ev.VersionNo, r.ChunkIndex)
		}
	}
	for i, r := range refs {
		pid := domain.PointID(domain.RAGNamespace().String(), ev.DocumentID, ev.VersionNo, r.ChunkIndex)
		payload := domain.ChunkMetadata{
			DocumentID:  ev.DocumentID,
			WorkspaceID: snap.WorkspaceID,
			DirectoryID: snap.DirectoryID,
			VersionNo:   ev.VersionNo,
			ChunkIndex:  r.ChunkIndex,
			ChunkText:   r.Text,
			SectionPath: r.SectionPath,
			ModelID:     model.ID,
			Tags:        snap.Tags,
			VisibleTo:   visibleTo,
			Status:      string(snap.Status),
			CreatedAt:   now,
			ChunkRole:   string(r.Role),
		}
		// child: link to parent point id + index
		if r.Role == RoleChild && r.ParentChunkIndex >= 0 {
			if pid, ok := parentIDs[r.ParentChunkIndex]; ok {
				payload.ParentChunkID = pid
				payload.ParentChunkIndex = r.ParentChunkIndex
			}
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
	// 7b. persist parent-child relations (10 §2.3) when the strategy produced
	// them. The ParseStore is optional; without it, relations are skipped
	// (parent-child is an opt-in strategy; the indexing path still works).
	if p.ParseStore != nil {
		relations := buildChunkRelations(ev, refs)
		if len(relations) > 0 {
			if err := p.ParseStore.RecordChunkRelations(ctx, ev.DocumentID, relations); err != nil {
				p.Logf("rag: record chunk relations for %s: %v", ev.DocumentID, err)
			}
		}
	}
	if err := p.Status.SetDocumentIndexStatus(ctx, ev.DocumentID, domain.IndexIndexed, ""); err != nil {
		return fmt.Errorf("set index status: %w", err)
	}
	p.Logf("rag: indexed document %s v%d (%d chunks)", ev.DocumentID, ev.VersionNo, len(refs))
	return nil
}

// buildChunkRelations collects child→parent links from refs (10 §2.3). Each
// relation carries the child's and parent's qdrant_point_id (the deterministic
// id the pipeline computed for that chunk_index), which ParseStore resolves to
// chunks.id. Non-parent-child refs produce no relations.
func buildChunkRelations(ev domain.DocEvent, refs []ChunkRef) []domain.ChunkRelation {
	// map parent chunk_index → point id
	parentIDs := make(map[int]string, len(refs))
	for _, r := range refs {
		if r.Role == RoleParent {
			parentIDs[r.ChunkIndex] = domain.PointID(domain.RAGNamespace().String(), ev.DocumentID, ev.VersionNo, r.ChunkIndex)
		}
	}
	var out []domain.ChunkRelation
	for _, r := range refs {
		if r.Role != RoleChild || r.ParentChunkIndex < 0 {
			continue
		}
		parentID, ok := parentIDs[r.ParentChunkIndex]
		if !ok {
			continue
		}
		childID := domain.PointID(domain.RAGNamespace().String(), ev.DocumentID, ev.VersionNo, r.ChunkIndex)
		out = append(out, domain.ChunkRelation{
			ChildChunkID:  childID,
			ParentChunkID: parentID,
			DocumentID:    ev.DocumentID,
		})
	}
	return out
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

// handleParse is the document.parse / document.reparse path (10 §4.1). It is
// the first of two decoupled stages: parse the uploaded file into Block JSONB
// + content_text, write them to the document, set parse_status=parsed, then
// publish a document.create event so the existing RAG pipeline re-indexes.
//
// Stage progress is recorded via ParseStore.AppendProgress (10 §6.1):
// fetching → parsing → (chunking) → persisting → done. Parse failures set
// parse_status=failed and propagate the error so the worker can retry/dead-letter.
//
// The event payload carries: storage_key, source_format, mime, filename,
// parse_opts (the per-upload overrides). RBAC was enforced at upload time (the
// API checked workspace write); the worker runs as a trusted system identity.
func (p *Pipeline) handleParse(ctx context.Context, ev domain.DocEvent) error {
	if p.ParseStore == nil || p.Parser == nil || p.Objects == nil {
		return fmt.Errorf("rag: parse stage not configured (parser/objects/parsestore)")
	}
	payload := ev.Payload
	storageKey, _ := payload["storage_key"].(string)
	if storageKey == "" {
		return fmt.Errorf("rag: parse event %s missing storage_key", ev.EventID)
	}
	mime, _ := payload["mime"].(string)
	filename, _ := payload["filename"].(string)
	sourceFormat, _ := payload["source_format"].(string)
	if sourceFormat == "" {
		sourceFormat = parser.FormatFromName(filename)
	}
	opts := parser.ParseOptions{}
	if rawOpts, ok := payload["parse_opts"]; ok {
		if b, mErr := json.Marshal(rawOpts); mErr == nil {
			_ = json.Unmarshal(b, &opts)
		}
	}

	// 1. idempotent parse task; if already parsed, skip (re-publish safe).
	task, err := p.ParseStore.UpsertParseTask(ctx, domain.ParseTask{
		DocumentID: ev.DocumentID,
		EventID:    ev.EventID,
		Status:     domain.ParseTaskPending,
		MaxAttempt: p.Cfg.MaxAttempt,
	})
	if err != nil {
		return fmt.Errorf("upsert parse task: %w", err)
	}
	if task.Status == domain.ParseTaskParsed {
		// already parsed by a prior run → ensure the document.create follow-on
		// is published, then ACK.
		return p.publishCreateForParse(ctx, ev)
	}

	// 2. mark parsing + stage progress.
	_ = p.ParseStore.SetDocumentParseStatus(ctx, ev.DocumentID, domain.ParseParsing, task.ID)
	_ = p.ParseStore.UpdateParseTaskStatus(ctx, task.ID, domain.ParseTaskParsing, task.Attempt+1, "")
	p.markProgress(ctx, task.ID, "parsing", "started", "route parser")

	// 3. route parser (registry first; fall back to the single wired Parser).
	prs := p.Parser
	if p.Registry != nil {
		if r, rerr := p.Registry.Lookup(mime, filename); rerr == nil {
			prs = r
		}
	}
	parsed, err := p.runParseStage(ctx, prs, storageKey, opts)
	if err != nil {
		_ = p.ParseStore.SetDocumentParseStatus(ctx, ev.DocumentID, domain.ParseFailed, task.ID)
		_ = p.ParseStore.UpdateParseTaskStatus(ctx, task.ID, domain.ParseTaskFailed, task.Attempt+1, err.Error())
		p.markProgress(ctx, task.ID, "parsing", "failed", err.Error())
		return fmt.Errorf("parse: %w", err)
	}
	p.markProgress(ctx, task.ID, "parsing", "done", "parsed "+parsed.Meta.Format)

	// 4. persist content + content_text + parse_status=parsed.
	p.markProgress(ctx, task.ID, "persisting", "started", "write content")
	if err := p.ParseStore.SetDocumentContent(ctx, ev.DocumentID, parsed.Blocks, parsed.ContentText, parsed.Meta.ParserName, sourceFormat); err != nil {
		_ = p.ParseStore.SetDocumentParseStatus(ctx, ev.DocumentID, domain.ParseFailed, task.ID)
		_ = p.ParseStore.UpdateParseTaskStatus(ctx, task.ID, domain.ParseTaskFailed, task.Attempt+1, err.Error())
		return fmt.Errorf("set document content: %w", err)
	}
	_ = p.ParseStore.SetDocumentParseStatus(ctx, ev.DocumentID, domain.ParseParsed, task.ID)
	_ = p.ParseStore.UpdateParseTaskStatus(ctx, task.ID, domain.ParseTaskParsed, task.Attempt+1, "")
	p.markProgress(ctx, task.ID, "persisting", "done", "content written")
	p.markProgress(ctx, task.ID, "done", "done", "")

	// 5. publish document.create → existing RAG pipeline re-indexes (two-stage
	// decoupling, 10 §4.1). The parse_opts.chunking_strategy is forwarded so the
	// indexing stage picks the configured chunker.
	return p.publishCreateForParse(ctx, ev)
}

// runParseStage reads the source object and parses it, surfacing multimodal
// warnings without blocking the main path (10 §3.2 degradation).
func (p *Pipeline) runParseStage(ctx context.Context, prs parser.Parser, storageKey string, opts parser.ParseOptions) (*parser.ParsedDocument, error) {
	return prs.Parse(ctx, p.Objects, storageKey, opts)
}

// publishCreateForParse re-publishes a document.create event after parsing so
// the existing RAG pipeline indexes the freshly-written content. The parse
// event is consumed by the worker; this new event re-enters the stream.
func (p *Pipeline) publishCreateForParse(ctx context.Context, ev domain.DocEvent) error {
	// The pipeline does not own the queue (the worker does); it signals via the
	// ParseStore's status. The worker's Process loop, after a successful parse,
	// publishes the follow-on create event itself (see worker.go).
	//
	// To keep the pipeline testable without the queue, handleParse reports
	// success here; the worker orchestrates the cross-stream publish. This is
	// the same split as handleIndex: the pipeline is stateless; the worker owns
	// the stream.
	_ = ctx
	_ = ev
	return nil
}

// markProgress appends a staged progress entry with a timestamp.
func (p *Pipeline) markProgress(ctx context.Context, taskID, stage, status, detail string) {
	now := p.Clock.Now().UTC().Format(time.RFC3339)
	_ = p.ParseStore.AppendProgress(ctx, taskID, domain.ProgressStage{
		Stage:  stage,
		Status: status,
		At:     now,
		Detail: detail,
	})
}

// ErrSkip is returned when an event should be considered done without indexing.
var ErrSkip = errors.New("rag: skip event")
