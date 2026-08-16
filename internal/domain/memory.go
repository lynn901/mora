package domain

import (
	"errors"
	"time"
)

// Phase 4 Agent memory value objects (design-docs/18 §2.1–2.6, §4).
// In-memory value objects for the memory-evidence/memory-units aggregate.
// Persistence lives in infra/postgres (memory_*.go). No business logic here —
// just the data shapes. Evidence ACL is independent of Memory publish (D2):
// publishing a Memory never writes a permissions(target_type='evidence') row
// (附录 A 不变量 8).

// EvidenceSourceKind is the origin class of an evidence record (12 §4.4).
type EvidenceSourceKind string

const (
	EvidenceSourceSession  EvidenceSourceKind = "session"
	EvidenceSourceMessage  EvidenceSourceKind = "message"
	EvidenceSourceToolCall EvidenceSourceKind = "tool_call"
	EvidenceSourceDocument EvidenceSourceKind = "document"
	EvidenceSourceCode     EvidenceSourceKind = "code"
)

// EvidenceVisibility is the default ACL shape of an evidence record (D2).
// session/message/tool_call evidence defaults to 'private' — only the owner
// can read it unless explicitly shared (11 §8.3).
type EvidenceVisibility string

const (
	EvidencePrivate     EvidenceVisibility = "private"
	EvidenceRestricted  EvidenceVisibility = "restricted"
)

// EvidenceState is the lifecycle state of an evidence record (D3, §9.2).
// active → pending_purge (expiry/disable, content still auditable) →
// purged (content erased, only id/hash/audit metadata remain).
type EvidenceState string

const (
	EvidenceActive       EvidenceState = "active"
	EvidencePendingPurge EvidenceState = "pending_purge"
	EvidencePurged       EvidenceState = "purged"
)

// EvidenceClassification is the auto-detected sensitivity label (§4.1).
type EvidenceClassification string

const (
	EvidenceClassSecret     EvidenceClassification = "secret"
	EvidenceClassCredential EvidenceClassification = "credential"
	EvidenceClassPII        EvidenceClassification = "pii"
	EvidenceClassNone       EvidenceClassification = "none"
)

// OwnerType is the principal kind that owns an evidence record (12 §4.4).
// Reused for memory_units.created_by_type and feedback.given_by_type.
type OwnerType string

const (
	OwnerUser           OwnerType = "user"
	OwnerGroup          OwnerType = "group"
	OwnerAgent          OwnerType = "agent"
	OwnerServiceAccount OwnerType = "service_account"
)

// MemoryEvidence is raw evidence L0 with an independent ACL (D2, §2.1).
// Small fragments (≤64KiB redacted) store AES-256-GCM ciphertext in
// EncryptedContent with the DEK envelope-wrapped by a versioned KEK; large
// objects store a MinIO key in StorageKey. Either EncryptedContent or
// StorageKey is set, never both. Purged rows clear both and retain only
// ContentHash + RedactedExcerpt + audit metadata (12 §8.4).
//
// CapturedAuthzRevision is audit-only and MUST NOT be used for future access
// authorization (12 §4.4).
type MemoryEvidence struct {
	ID                       UUID
	WorkspaceID              UUID
	OwnerType                OwnerType
	OwnerID                  UUID
	SourceKind               EvidenceSourceKind
	SourceRef                string
	SourceAssetID            *UUID // references knowledge_assets(id), no FK — deletion is propagated, not cascaded
	SourceAssetVersionID     *UUID
	Visibility               EvidenceVisibility
	CapturedAuthzRevision    int64
	ContentHash              string
	EncryptedContent         []byte // small fragment ciphertext; nil when StorageKey is set
	StorageKey               string // large-object MinIO key mora-evidence/<ws>/<id>
	KeyVersion               *int   // envelope KEK version; required when EncryptedContent != nil
	RedactedExcerpt          string
	Classification           EvidenceClassification
	RetentionPolicyID        *UUID
	State                    EvidenceState
	CreatedAt                time.Time
	ExpiresAt                *time.Time
	PurgedAt                 *time.Time
	DeletedAt                *time.Time
}

// MemoryType is the kind of a distilled memory unit (12 §4.4).
type MemoryType string

const (
	MemoryFact        MemoryType = "fact"
	MemoryDecision    MemoryType = "decision"
	MemoryConstraint  MemoryType = "constraint"
	MemoryPreference  MemoryType = "preference"
	MemoryEvent       MemoryType = "event"
)

// MemoryUnitState is the lifecycle state of a memory unit (§6.2).
// candidate → approved → published (or rejected/deprecated). Published requires
// a review_decision; first version has NO auto-publish (附录 A 不变量 9).
type MemoryUnitState string

const (
	MemoryCandidate  MemoryUnitState = "candidate"
	MemoryApproved   MemoryUnitState = "approved"
	MemoryPublished  MemoryUnitState = "published"
	MemoryRejected   MemoryUnitState = "rejected"
	MemoryDeprecated MemoryUnitState = "deprecated"
)

// MemoryUnit is a distilled structured memory record, attached to a
// knowledge_assets(asset_type='memory') row via AssetID (D1, §2.2).
// SupersededBy is written only after a reviewer confirms merge/supersede;
// dedup/conflict suggestions land in MemoryDedupSuggestion first (§5.2) and
// never mutate this field directly. EvidenceMissing=true units are NOT used as
// high-authority recall (12 §8.4) but their redacted reference + verification
// status remain readable.
type MemoryUnit struct {
	ID                UUID
	WorkspaceID       UUID
	AssetID           UUID
	AssetVersionID    *UUID
	MemoryType        MemoryType
	Statement         string
	StructuredPayload map[string]any
	Confidence        *float64
	ValidFrom         *time.Time
	ExpiresAt         *time.Time
	State             MemoryUnitState
	SupersededBy      *UUID
	EvidenceMissing   bool
	Authority         float64
	CreatedByType     OwnerType
	CreatedByID       UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// SupportType is the relation of an evidence link to a memory unit (§2.3).
type SupportType string

const (
	Supports     SupportType = "supports"
	Contradicts SupportType = "contradicts"
)

// MemoryEvidenceLink is the M:N join between a memory unit and an evidence
// record (§2.3). QuoteLocator is a non-executable reference (offset/range/hash)
// that never carries the original text.
type MemoryEvidenceLink struct {
	MemoryUnitID  UUID
	EvidenceID    UUID
	QuoteLocator  map[string]any
	SupportType   SupportType
	CreatedAt     time.Time
}

// RetentionPolicy governs evidence retention per workspace + memory_type
// (D3, §2.4). Specific duration values are a PM governance decision (§19.6);
// this struct only carries the shape. A NULL MemoryType means the workspace
// default across all types.
type RetentionPolicy struct {
	ID           UUID
	WorkspaceID  UUID
	MemoryType   *MemoryType
	RetainFor    time.Duration
	PurgeAfter   *time.Duration
	IsSystem     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// FeedbackType is the human/agent signal on a memory unit (D8).
// Feedback never edits the statement — it only adjusts authority/freshness
// and may trigger a revalidate Job for incorrect/stale.
type FeedbackType string

const (
	FeedbackUseful    FeedbackType = "useful"
	FeedbackIncorrect FeedbackType = "incorrect"
	FeedbackStale     FeedbackType = "stale"
)

// MemoryFeedback is a useful/incorrect/stale signal on a memory unit (§2.5).
type MemoryFeedback struct {
	ID                   UUID
	MemoryUnitID         UUID
	FeedbackType         FeedbackType
	GivenByType          OwnerType
	GivenByID            UUID
	RationaleRedacted    string
	RevalidateTriggered  bool
	CreatedAt            time.Time
}

// DedupSuggestionType is the relation a dedup suggestion proposes (D7).
// These are SUGGESTIONS only — never auto-merge (附录 A 不变量 9).
type DedupSuggestionType string

const (
	DedupDuplicate    DedupSuggestionType = "duplicate"
	DedupExtends      DedupSuggestionType = "extends"
	DedupContradicts DedupSuggestionType = "contradicts"
	DedupUnrelated    DedupSuggestionType = "unrelated"
)

// DedupSuggestionOrigin is how a dedup suggestion was produced (§2.6).
type DedupSuggestionOrigin string

const (
	DedupOriginRule      DedupSuggestionOrigin = "rule"
	DedupOriginGenerated DedupSuggestionOrigin = "generated"
)

// DedupSuggestionState is the reviewer disposition of a suggestion (§2.6).
type DedupSuggestionState string

const (
	DedupPending  DedupSuggestionState = "pending"
	DedupAccepted DedupSuggestionState = "accepted"
	DedupRejected DedupSuggestionState = "rejected"
)

// MemoryDedupSuggestion is a non-merging dedup/conflict suggestion (D7, §2.6).
// A reviewer's disposition writes MemoryUnit.SupersededBy (duplicate/extends)
// or a knowledge_relations(relation_type='contradicts') row; the suggestion
// itself never mutates the unit.
type MemoryDedupSuggestion struct {
	ID              UUID
	WorkspaceID     UUID
	UnitAID         UUID
	UnitBID         UUID
	SuggestionType  DedupSuggestionType
	Origin          DedupSuggestionOrigin
	Confidence      *float64
	EvidenceRef     map[string]any
	State           DedupSuggestionState
	ResolvedByType  *OwnerType
	ResolvedByID    *UUID
	ResolvedAt      *time.Time
	CreatedAt       time.Time
}

// EvidenceWriteOpts controls storage split (D4, §4.2): small fragments are
// AES-256-GCM encrypted into EncryptedContent; large objects are written to
// MinIO and only StorageKey + ContentHash + RedactedExcerpt persist in PG.
// The threshold matches the design's 64KiB redacted-content cutoff.
const EvidenceInlineMaxBytes = 64 * 1024 // 64 KiB — §4.2 small/large split

// Memory phase sentinel errors. These let the service layer classify outcomes
// without re-reading state (same pattern as the §7 CAS sentinels).
var (
	// ErrEvidenceNotFound: the evidence id does not resolve or is not visible
	// to the caller. Indistinguishable from a permission denial so existence is
	// never leaked (§9.3).
	ErrEvidenceNotFound = errors.New("memory: evidence not found or not visible")
	// ErrMemoryUnitNotFound: the memory unit id does not resolve or is not
	// visible to the caller (§9.3).
	ErrMemoryUnitNotFound = errors.New("memory: unit not found or not visible")
)
