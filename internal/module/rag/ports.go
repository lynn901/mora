// Package rag owns the Automated RAG & Vector engine (PRD §4 模块二 / YS-8).
//
// ports.go defines every external capability the RAG module needs. The RAG
// pipeline, search engine, worker and HTTP handlers depend only on these
// interfaces, never on concrete infra. This is the "mock-first" contract: the
// Mora backend (YS-6) supplies the concrete RBAC / document / FTS / index-status
// repositories, infra supplies Qdrant + Valkey clients, and tests supply
// in-memory fakes — so the engine is fully exercised without a database.
package rag

import (
	"context"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag/provider"
)

// ---------------------------------------------------------------------------
// Document access (owned by YS-6 Mora backend; RAG consumes read-only snapshots)
// ---------------------------------------------------------------------------

// DocumentSnapshot is the immutable input to the pipeline for one document
// version. Content is the raw Block JSONB; the pipeline extracts text from it.
type DocumentSnapshot struct {
	DocumentID  string
	WorkspaceID string
	DirectoryID string
	Title       string
	Content     []byte // Block JSONB array
	ContentText string // pre-extracted plain text (documents.content_text), may be empty
	Format      string // blocks | markdown
	VersionNo   int
	Status      domain.DocStatus
	Tags        []string
	// Document-parsing (10 §4). StorageKey is the object-storage key of the
	// uploaded source file; SourceFormat is its canonical format key. The parse
	// worker reads these to know which file to parse.
	StorageKey   string
	SourceFormat string
}

// DocumentStore loads document snapshots for indexing. Owned by YS-6.
type DocumentStore interface {
	GetSnapshot(ctx context.Context, documentID string, versionNo int) (DocumentSnapshot, error)
	// PublishedDocumentIDs lists published document ids, paged by cursor, for
	// full rebuilds (model switch). Returns ids and a next cursor ("" = done).
	PublishedDocumentIDs(ctx context.Context, cursor string, limit int) (ids []string, next string, err error)
}

// ---------------------------------------------------------------------------
// RBAC (owned by YS-6 platform/rbac; RAG uses it for visible_to computation
// and search-time hard filtering). 05-rag-pipeline-design.md §4.3.
// ---------------------------------------------------------------------------

// ViewerScope is the search-time permission envelope for a user: the subject
// ids ("user:<id>" + "group:<id>" for every group membership) that must
// intersect a chunk's visible_to, plus the workspace scoping.
type ViewerScope struct {
	UserID       string
	SubjectIDs   []string // ["user:<id>", "group:<g1>", ...]
	WorkspaceIDs []string // workspaces the user may read (empty = all visible)
	DirectoryIDs []string // optional directory scoping (empty = no dir filter)
}

// RBACResolver resolves read visibility. Owned by YS-6 platform/rbac.
type RBACResolver interface {
	// ResolveReaders returns the subject ids (user:/group:) that may read the
	// document — used to populate the chunk visible_to payload at index time.
	// Deny > Allow > Inherit > default-deny (PRD F1.4).
	ResolveReaders(ctx context.Context, documentID string) ([]string, error)
	// ViewerScope computes the search-time envelope for a user.
	ViewerScope(ctx context.Context, userID string) (ViewerScope, error)
}

// ---------------------------------------------------------------------------
// Vector store (Qdrant). 03-data-model.md §3 / 05 §4.
// ---------------------------------------------------------------------------

// VectorPoint is a chunk vector + payload written to Qdrant.
type VectorPoint struct {
	PointID string
	Vector  []float32
	Payload domain.ChunkMetadata
}

// VectorSearchRequest is a Dense search with an RBAC hard filter baked in.
type VectorSearchRequest struct {
	CollectionName string
	Vector         []float32
	TopK           int
	WorkspaceID    string   // optional workspace scope
	VisibleTo      []string // subject ids that must intersect chunk.visible_to (HARD FILTER)
	DirectoryID    string   // optional directory scope
	Tags           []string // optional tag filter
}

// VectorHit is a raw Dense retrieval result before fusion.
type VectorHit struct {
	PointID string
	Score   float32
	Payload domain.ChunkMetadata
}

// VectorStore is the Qdrant abstraction.
type VectorStore interface {
	// EnsureCollection creates the collection (dense+sparse) + payload indexes
	// if absent. Idempotent.
	EnsureCollection(ctx context.Context, name string, dim int) error
	UpsertChunks(ctx context.Context, collection string, points []VectorPoint) error
	// DeleteByDocument removes every point for a document (all versions) — cascade delete.
	DeleteByDocument(ctx context.Context, collection, documentID string) error
	// DeleteByDocumentVersion removes points for one (document,version) — update cascade.
	DeleteByDocumentVersion(ctx context.Context, collection, documentID string, versionNo int) error
	// SetVisibleTo updates the visible_to payload of every chunk for a document
	// (permission change recompute). 05 §4.3.3.
	SetVisibleTo(ctx context.Context, collection, documentID string, visibleTo []string) error
	SearchDense(ctx context.Context, req VectorSearchRequest) ([]VectorHit, error)
}

// ---------------------------------------------------------------------------
// Full-text search (PostgreSQL FTS / BM25). Owned by YS-6.
// ---------------------------------------------------------------------------

// FTSHit is a BM25 retrieval result before fusion.
type FTSHit struct {
	DocumentID  string
	Title       string
	ChunkText   string // snippet (ts_headline)
	ChunkIndex  int
	Score       float32
	WorkspaceID string
}

// FTSStore runs BM25 retrieval with RBAC SQL filtering (existence not leaked).
type FTSStore interface {
	SearchBM25(ctx context.Context, req FTSRequest) ([]FTSHit, error)
}

// FTSRequest carries the query + RBAC envelope for BM25 retrieval.
type FTSRequest struct {
	Query       string
	TopK        int
	WorkspaceID string
	DirectoryID string
	VisibleTo   []string // RBAC subjects (SQL-layer hard filter)
}

// ---------------------------------------------------------------------------
// Index status / metadata store (PostgreSQL). Owned by YS-6; RAG writes receipts.
// ---------------------------------------------------------------------------

// IndexStatusStore persists the pipeline state machine + chunk metadata and
// writes the index_status receipt back to documents.
type IndexStatusStore interface {
	// UpsertTask creates the task if absent (idempotent on document_id+event_id)
	// and returns the current row.
	UpsertTask(ctx context.Context, task domain.IndexingTask) (domain.IndexingTask, error)
	UpdateTaskStatus(ctx context.Context, taskID string, status domain.IndexingTaskStatus, attempt int, errMsg string) error
	GetTask(ctx context.Context, taskID string) (domain.IndexingTask, error)
	// SetDocumentIndexStatus writes the knowledge-index badge (documents.index_status).
	SetDocumentIndexStatus(ctx context.Context, documentID string, status domain.IndexStatus, errMsg string) error
	GetDocumentIndexStatus(ctx context.Context, documentID string) (IndexStatusInfo, error)
	// RecordChunks persists chunk metadata rows (vector body is in Qdrant).
	RecordChunks(ctx context.Context, documentID string, versionNo int, modelID string, chunks []domain.Chunk) error
	// DeleteChunkMeta removes chunk metadata rows for one version (update cascade bookkeeping).
	DeleteChunkMeta(ctx context.Context, documentID string, versionNo int) error
	// DeleteAllChunkMeta removes all chunk metadata rows for a document (delete cascade).
	DeleteAllChunkMeta(ctx context.Context, documentID string) error
	// PendingTasks returns tasks needing retry/compensation (status pending|failed,
	// updated before cutoff) — for the compensation scanner.
	PendingTasks(ctx context.Context, cutoff time.Time, limit int) ([]domain.IndexingTask, error)
}

// IndexStatusInfo is the document index badge read model (API 04 §9.1).
type IndexStatusInfo struct {
	IndexStatus   domain.IndexStatus
	LastIndexedAt *time.Time
	ChunkCount    int
	LastError     string
}

// ---------------------------------------------------------------------------
// Event queue (Valkey Streams). 05 §2.
// ---------------------------------------------------------------------------

// QueueMessage is one delivered stream entry.
type QueueMessage struct {
	Stream    string
	ID        string // stream entry id (e.g. "1234-0")
	DocEvent  domain.DocEvent
	RawFields map[string]string
}

// EventQueue is the Valkey Streams abstraction (stream: doc_events, group:
// rag_pipeline_group). Dead-letter stream: doc_events:dead.
type EventQueue interface {
	Publish(ctx context.Context, ev domain.DocEvent) (entryID string, err error)
	// ReadGroup blocks reading up to count messages for the consumer.
	ReadGroup(ctx context.Context, consumer string, count int64, block time.Duration) ([]QueueMessage, error)
	Ack(ctx context.Context, msg QueueMessage) error
	MoveToDeadLetter(ctx context.Context, msg QueueMessage, reason string) error
	// Claim steals idle messages (idle > minIdle) for crash recovery (XAUTOCLAIM).
	Claim(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]QueueMessage, error)
}

// IdempotencyStore deduplicates events by event_id (Valkey SET, TTL 24h).
type IdempotencyStore interface {
	// MarkSeen returns true if the event was already seen (and is now marked).
	MarkSeen(ctx context.Context, eventID string, ttl time.Duration) (bool, error)
}

// ParseTaskStore persists the parse_tasks state machine, the staged progress
// timeline, and the documents.parse_status badge (10 §4.2.2, §6). It is the
// parse-side analogue of IndexStatusStore: the worker owns idempotency +
// retry, the pipeline.ParseStage does the actual parsing.
type ParseTaskStore interface {
	// UpsertParseTask creates the task if absent (idempotent on
	// document_id+event_id) and returns the current row.
	UpsertParseTask(ctx context.Context, task domain.ParseTask) (domain.ParseTask, error)
	UpdateParseTaskStatus(ctx context.Context, taskID string, status domain.ParseTaskStatus, attempt int, errMsg string) error
	// AppendProgress adds a staged progress entry to the timeline (10 §6.1).
	AppendProgress(ctx context.Context, taskID string, stage domain.ProgressStage) error
	GetParseTask(ctx context.Context, taskID string) (domain.ParseTask, error)
	// SetDocumentParseStatus writes the documents.parse_status badge.
	SetDocumentParseStatus(ctx context.Context, documentID string, status domain.ParseStatus, taskID string) error
	// SetDocumentContent writes parsed content (Block JSONB) + content_text +
	// the parser_name metadata back to the document row (10 §4.1 step 3).
	SetDocumentContent(ctx context.Context, documentID string, blocks []byte, contentText, parserName, sourceFormat string) error
	// GetParseProgress returns the staged timeline + statuses for the
	// parse-progress query API (10 §6.3). Existence-non-leak is the caller's
	// responsibility (RBAC read check first).
	GetParseProgress(ctx context.Context, documentID string) (ParseProgressInfo, error)
	// RecordChunkRelations persists parent-child links for a version (10 §2.3).
	// child/parent ids are the chunks table PKs; the caller resolves them from
	// the chunk metadata rows recorded by IndexStatusStore.RecordChunks.
	RecordChunkRelations(ctx context.Context, documentID string, relations []domain.ChunkRelation) error
}

// ParseProgressInfo is the parse-status read model for the parse-progress API
// (10 §6.3). Carries both the parse and index badges so the UI can show the
// full two-stage status.
type ParseProgressInfo struct {
	ParseStatus domain.ParseStatus
	IndexStatus domain.IndexStatus
	Progress    []domain.ProgressStage
	UpdatedAt   *time.Time
}

// ChunkRelation is one parent-child link (10 §2.3); stored in chunk_relations.
type ChunkRelation struct {
	ChildChunkID  string
	ParentChunkID string
	DocumentID    string
}

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the default Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// ---------------------------------------------------------------------------
// Embedding model registry + provider factory.
// ---------------------------------------------------------------------------

// ModelStore persists embedding_models config (03-data-model.md §2.7). The
// admin handlers (04 §9.2) CRUD through this; the pipeline reads the active one.
type ModelStore interface {
	GetActive(ctx context.Context) (domain.EmbeddingModel, error)
	GetByID(ctx context.Context, id string) (domain.EmbeddingModel, error)
	List(ctx context.Context) ([]domain.EmbeddingModel, error)
	Upsert(ctx context.Context, m domain.EmbeddingModel) (domain.EmbeddingModel, error)
	SetActive(ctx context.Context, id string) error
}

// ProviderFactory builds an EmbeddingProvider / Reranker for a model config.
// Production picks TEI vs Ollama by model.Provider; tests inject a fake.
type ProviderFactory interface {
	For(ctx context.Context, model domain.EmbeddingModel) (provider.EmbeddingProvider, error)
	Reranker(ctx context.Context) (provider.RerankerProvider, error)
}
