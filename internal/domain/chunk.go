// Package domain defines cross-module domain types shared by the RAG engine.
//
// These types mirror the YS-5 data model (03-data-model.md §2.7, §3.2) and the
// RAG pipeline contract (05-rag-pipeline-design.md). They are intentionally
// framework-free so the RAG module can be built and tested mock-first while the
// Mora backend (YS-6) supplies the concrete repositories.
package domain

import (
	"strings"
	"time"
)

// NOTE: IndexStatus / IndexPending / IndexProcessing / IndexIndexed / IndexFailed
// are defined in user.go (owned by the Mora backend, YS-6) and reused by RAG.

// DocStatus mirrors documents.status.
type DocStatus string

const (
	DocDraft     DocStatus = "draft"
	DocPublished DocStatus = "published"
	DocArchived  DocStatus = "archived"
	DocDeleted   DocStatus = "deleted"
)

// EmbeddingModel mirrors the embedding_models table (03-data-model.md §2.7).
type EmbeddingModel struct {
	ID               string
	Provider         string // tei | ollama
	ModelName        string
	Dimension        int
	MaxToken         int
	InstructionQuery string // query 前缀（Instruction-Aware）
	InstructionDoc   string // doc 前缀
	Status           string // active | inactive
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// collectionPrefix is the Qdrant collection-name prefix. It defaults to
// "mora_chunks_" (brand-naming-spec §4-F) and is configurable at process start
// via SetCollectionPrefix so future renames cost nothing (PM suggestion,
// adopted in YS-49). The domain package stays env-free; binaries inject the
// configured value from env (e.g. RAG_COLLECTION_PREFIX) at startup.
var collectionPrefix = "mora_chunks_"

// SetCollectionPrefix overrides the Qdrant collection-name prefix. It must be
// called once at process start, before any indexing/search runs, so that all
// callers of CollectionName agree on the same collection. Leading/trailing
// spaces are trimmed; an empty value is ignored to keep the default safe.
func SetCollectionPrefix(prefix string) {
	if p := strings.TrimSpace(prefix); p != "" {
		collectionPrefix = p
	}
}

// CollectionPrefix returns the currently configured collection-name prefix.
func CollectionPrefix() string { return collectionPrefix }

// CollectionName returns the deterministic Qdrant collection name for a model.
// One collection per model; dimension changes require a new collection.
func (m EmbeddingModel) CollectionName() string {
	return collectionPrefix + slug(m.Provider) + "_" + slug(m.ModelName) + "_" + itoa(m.Dimension)
}

// Chunk mirrors the chunks table (vector body lives in Qdrant; this is metadata
// for reconciliation / cascade cleanup).
type Chunk struct {
	ID            string
	DocumentID    string
	VersionNo     int
	ChunkIndex    int
	Text          string
	TokenCount    int
	SectionPath   string
	ModelID       string
	QdrantPointID string
	Metadata      ChunkMetadata
	CreatedAt     time.Time
}

// ChunkMetadata is stored in both the chunks.metadata JSONB column and the
// Qdrant point payload (03-data-model.md §3.2). visible_to is the RBAC core.
type ChunkMetadata struct {
	WorkspaceID string   `json:"workspace_id"`
	DirectoryID string   `json:"directory_id,omitempty"`
	VersionNo   int      `json:"version_no"`
	ChunkIndex  int      `json:"chunk_index"`
	ChunkText   string   `json:"chunk_text"`
	SectionPath string   `json:"section_path,omitempty"`
	ModelID     string   `json:"model_id"`
	Tags        []string `json:"tags,omitempty"`
	VisibleTo   []string `json:"visible_to"` // subject ids: "user:<id>" / "group:<id>"
	Status      string   `json:"status"`     // document status snapshot
	DocumentID  string   `json:"document_id"`
	CreatedAt   string   `json:"created_at"`
}

// IndexingTask mirrors the indexing_tasks table (state machine).
type IndexingTask struct {
	ID           string
	DocumentID   string
	EventID      string
	EventType    EventType
	Status       IndexingTaskStatus
	Attempt      int
	MaxAttempt   int
	Payload      map[string]any
	ErrorMessage string
	ModelID      string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type IndexingTaskStatus string

const (
	TaskPending    IndexingTaskStatus = "pending"
	TaskProcessing IndexingTaskStatus = "processing"
	TaskIndexed    IndexingTaskStatus = "indexed"
	TaskFailed     IndexingTaskStatus = "failed"
)
