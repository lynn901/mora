package postgres

// memory_recall_adapters.go declares that the existing memory infra repos
// also satisfy the recall-package narrow read ports (design-docs/18 §3.2
// dependency rule — recall depends on narrow ports, not full CRUD repos).
//
//   - MemoryEvidenceRepo.Get         → recall.EvidenceReader.Get
//   - MemoryEvidenceLinkRepo.ListForUnit → recall.LinkReader.ListForUnit
//
// No new methods: the signatures already match. The compile-time assertions
// below pin the contract so a future drift in either repo breaks the build
// here, not at a runtime call site.
import (
	"github.com/lynn901/mora/internal/module/memory/recall"
)

var _ recall.EvidenceReader = (*MemoryEvidenceRepo)(nil)
var _ recall.LinkReader = (*MemoryEvidenceLinkRepo)(nil)

// NewRecallEvidenceReader builds the concrete *MemoryEvidenceRepo so it satisfies
// BOTH evidence.EvidenceRepo (write/capture path) AND recall.EvidenceReader
// (the §4.3 read path) — the interface-returning NewMemoryEvidenceRepo would
// hide the concrete type and force a runtime type assertion at the wiring site.
func NewRecallEvidenceReader(db *DB) *MemoryEvidenceRepo { return &MemoryEvidenceRepo{db: db} }

// NewRecallLinkReader builds the concrete *MemoryEvidenceLinkRepo so it satisfies
// BOTH evidence.EvidenceLinkRepo AND recall.LinkReader (§8.1 citation resolution).
func NewRecallLinkReader(db *DB) *MemoryEvidenceLinkRepo { return &MemoryEvidenceLinkRepo{db: db} }

// NewRecallFeedbackRepo builds the concrete *FeedbackRepo so it satisfies BOTH
// evidence.FeedbackRepo AND recall.FeedbackRepo (the §8.3 narrow write port).
func NewRecallFeedbackRepo(db *DB) *FeedbackRepo { return &FeedbackRepo{db: db} }
