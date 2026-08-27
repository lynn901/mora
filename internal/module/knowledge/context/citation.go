// citation.go — Citation 结构（§8.1）+ CitationBuilder 签名（§8.2）。
//
// The Citation is the traceable reference a candidate carries (11 §7.4). The
// CitationBuilder finalizes it AFTER authorization (§8.2 step 5 post-check),
// mapping the per-type sub-structures the candidates already carry (memory
// evidence locator, code file:line, document block_id) into the unified
// Citation fields — no re-resolution (§8.2). ProjectionRef is internal
// diagnostic only and is NOT returned to the Agent (§8.2, inherited from the
// Phase 4 recall constraint).

package contextbroker

import (
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// Citation is the unified traceable reference (11 §7.4 / §8.1). SourceRef is
// the redacted source name/url (no credentials); VersionOrRevision is the
// version anchor per type (document=version_id, codebase=commit, memory=
// evidence_id, skill=package_version); Locator is the precise position
// (document block / file:line / session message / skill resource).
type Citation struct {
	AssetID           uuid.UUID
	AssetType          domain.AssetType
	SourceRef          string // source name/url (redacted; no credentials)
	VersionOrRevision  string // document: version_id; codebase: commit; memory: evidence_id; skill: package_version
	UpdatedAt          time.Time
	Authority          float64
	Confidence         *float64
	Locator            map[string]any // precise position: block / file:line / message / resource
}

// CitationBuilder finalizes citations after authorization (§8.2). It maps the
// per-type sub-structures candidates already carry — it does NOT re-resolve
// (§8.2). ProjectionRef stays internal-diagnostic-only and is never returned
// to the Agent (§8.2 / Phase 4 recall constraint).
//
// Implementations land in a follow-up sub-task; the signature is fixed here.
type CitationBuilder interface {
	Build(candidates []KnowledgeCandidate) []KnowledgeCandidate
}
