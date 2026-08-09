// Package parser implements the multi-format document parsing layer
// (design-docs/10-document-parsing-design.md §1). It converts an uploaded file
// (read from object storage) into a ParsedDocument whose Blocks reuse the
// documents.content Block JSONB schema, so the existing RAG pipeline (extract →
// chunk → embed → Qdrant) consumes the output unchanged.
//
// Design principles (§1.1):
//   - Pure-Go first: text/structured formats use stdlib + MIT/BSD libs, no CGO.
//   - Pluggable: a Parser interface is registered per MIME/extension; adding a
//     format adds one implementation, never a pipeline change.
//   - AGPL stays out of the main process: complex PDF/PPT/OCR/VLM/ASR go to the
//     optional mora-parser sidecar (P2), not the Go binaries.
//   - Decoupled: the parser emits ParsedDocument; it never writes Qdrant.
package parser

import "context"

// ParsedDocument is the output of a Parser: structured content the existing
// pipeline already consumes (Block JSONB) + plain text for FTS + extracted
// assets (images/tables) referenced by block id (§1.2).
type ParsedDocument struct {
	Blocks      []byte        // Block JSONB array, same schema as documents.content
	ContentText string        // plain text, written to documents.content_text for FTS
	Title       string        // extracted document title (heading or metadata)
	Assets      []ParsedAsset // images/figures/tables extracted for multimodal (P2)
	Meta        ParsedMeta
}

// ParsedAsset links an extracted image/table/chart to its placeholder block.
type ParsedAsset struct {
	BlockID    string // links the asset to its placeholder block
	Kind       string // image / table / chart
	MIMEType   string
	StorageKey string // object-storage key (parsed assets persist like attachments)
}

// ParsedMeta records how a document was parsed (§1.2).
type ParsedMeta struct {
	Format     string // pdf / docx / ...
	ParserName string // "ledongthuc/pdf" / "docx-self" / "mora-parser"
	PageCount  int
	Warnings   []string
	DurationMS int64
}

// ParseOptions carries per-upload config overrides (§7). Zero values mean
// "inherit from workspace/global default" — resolved by the caller before Parse.
type ParseOptions struct {
	ChunkingStrategy string `json:"chunking_strategy,omitempty"` // fixed / adaptive_3tier / parent_child
	ChunkSize        int    `json:"chunk_size,omitempty"`
	ChunkOverlap     int    `json:"chunk_overlap,omitempty"`
	EnableOCR        bool   `json:"enable_ocr,omitempty"`   // scanned PDF / image OCR (P2)
	EnableVLM        bool   `json:"enable_vlm,omitempty"`   // image description (P2)
	EnableASR        bool   `json:"enable_asr,omitempty"`   // audio/video transcription (P2)
	EnableGraph      bool   `json:"enable_graph,omitempty"` // graph extraction (P2)
	EnableQAGen      bool   `json:"enable_qagen,omitempty"` // question generation (P2)
	OcrLang          string `json:"ocr_lang,omitempty"`     // chi_sim+eng / eng
	VLMModel         string `json:"vlm_model,omitempty"`    // ollama model id, e.g. minicpm-v
}

// Reader is the byte source a Parser reads from. In production this is the
// object-storage abstraction (MinIO); tests supply an in-memory reader. The
// parser never touches a filesystem path — it reads via this interface so the
// sidecar path (fetch by storage key) and the test path (bytes) share one shape.
type Reader interface {
	// Read returns the raw bytes for storageKey. Implementations may stream.
	Read(ctx context.Context, storageKey string) ([]byte, error)
}

// Parser converts a raw uploaded file into a ParsedDocument. Implementations are
// registered per MIME/extension (§1.2).
type Parser interface {
	// Parse reads the object at storageKey via r and returns structured content.
	Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error)
	// Supports reports whether this parser handles the given MIME / filename.
	Supports(mime, filename string) bool
	// Name identifies the parser in metadata and metrics.
	Name() string
}
