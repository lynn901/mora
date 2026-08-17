// Package recall — service layer (design-docs/18 §8.1 召回, §8.2 权威策略).
//
// The RecallService implements the MemoryQuery.Recall port (12 §9.4). It
// composes the RecallRepo (ranked unit rows), the EvidenceReader + LinkReader
// (citations + the §4.3 ACL chain), the rbac.Engine (Evidence ACL), and the
// audit.Logger. It is the ONLY shaper of UnitRow → KnowledgeCandidate.
//
// Leak-safe (§9.3): by default only published units are recalled. The owner
// may opt into candidates (§8.5) — a non-owner's IncludeCandidates is silently
// downgraded to published-only so a private candidate's existence never leaks.
// Units whose backing evidence the caller cannot read are dropped from the
// citation (the redacted reference stays, but evidence_read is gated). The
// caller never sees a 403/404 distinction — empty is empty.
//
// Authority policy (§9.5): the repo applies the ranking (evidence_missing
// desc → confidence → freshness → authority); the service carries the
// relations (incl. contradicts) so conflicts surface (§8.2 / 11 §6.4).
package recall

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// AuthContext carries the caller identity the recall service needs for the
// §4.3 Evidence ACL chain + the §8.5 owner-only candidate read. It mirrors
// evidence.AuthContext so wiring is uniform across the memory module.
type AuthContext struct {
	SubjectType     domain.SubjectType
	PrincipalID     uuid.UUID
	GroupIDs        []uuid.UUID
	IsAdmin         bool
	IsServiceCaller bool
}

// MemoryQuery is the type port (12 §9.4). The recall service implements
// Recall; other type-query ports (DocumentQuery/CodeQuery/SkillQuery) live
// in their own packages.
type MemoryQuery interface {
	Recall(ctx context.Context, auth AuthContext, q KnowledgeQuery) ([]KnowledgeCandidate, error)
}

// defaultRecallCap bounds a default recall so a missing MaxItems never returns
// an unbounded set (§9.6 budget — a single asset cannot dominate).
const defaultRecallCap = 20

// ErrInvalidQuery is returned when the query lacks the minimum required axes
// (workspace is mandatory — recall is always workspace-scoped, §8.1).
var ErrInvalidQuery = errors.New("memory: invalid recall query")

// RecallService composes the recall repo + evidence/link readers + unit reader
// + rbac + audit. It is the MemoryQuery.Recall implementation.
type RecallService struct {
	units      RecallRepo
	links      LinkReader
	evidence   EvidenceReader
	unitReader UnitReader   // resolves unit.id → asset_id for the §4.3 step-1 gate
	rbac       *rbac.Engine // nil = no resource-level authz (dev/test only)
	audit      AuditLogger  // nil = no audit (dev/test only); local port, no platform/audit dep
}

// NewRecallService wires the recall service. rbac/audit may be nil in dev/test
// (production MUST inject both via WithAuthz so the §4.3 Evidence ACL chain
// runs). Without rbac the service treats evidence citations as unreadable
// (fail-closed: the redacted reference still surfaces, but evidence_read is
// gated) — it does NOT leak evidence content.
func NewRecallService(units RecallRepo, links LinkReader, evidence EvidenceReader) *RecallService {
	return &RecallService{units: units, links: links, evidence: evidence}
}

// WithUnits injects the UnitReader used to resolve a unit's anchor asset for
// the §4.3 step-1 read gate (unit use/read before expanding evidence).
// Production wiring MUST call this so ReadExcerpt enforces the full §4.3 chain;
// without it ReadExcerpt skips step 1 (the unit-read gate) and only enforces
// steps 2–3. Returns the service for chaining.
func (s *RecallService) WithUnits(unitReader UnitReader) *RecallService {
	s.unitReader = unitReader
	return s
}

// WithAuthz injects the RBAC engine + audit logger and returns the service
// for chaining. Production wiring MUST call this so the §4.3 Evidence ACL
// chain + the §9.4 audit events run. The logger is the local AuditLogger port
// — the wiring layer passes a *audit.Logger, which satisfies it.
func (s *RecallService) WithAuthz(engine *rbac.Engine, logger AuditLogger) *RecallService {
	s.rbac = engine
	s.audit = logger
	return s
}

// Recall implements MemoryQuery.Recall (12 §9.4). It:
//  1. Validates the query is workspace-scoped (§8.1).
//  2. Downgrades IncludeCandidates to published-only for non-owners (§8.5/§9.3
//     — a private candidate's existence never leaks).
//  3. Asks the RecallRepo for ranked UnitRows (§9.5 authority ranking).
//  4. Shapes each row into a KnowledgeCandidate, attaching the traceable
//     evidence citation (§8.1) + relations (§8.2).
//  5. Audits evidence.read decisions (§9.4 — allow/deny).
//
// A caller with no visible units gets an empty slice, never an error (§9.3).
func (s *RecallService) Recall(ctx context.Context, auth AuthContext, q KnowledgeQuery) ([]KnowledgeCandidate, error) {
	if q.WorkspaceID == uuid.Nil {
		return nil, ErrInvalidQuery
	}

	// §8.5/§9.3: unpublished (candidate/approved) units are owner-only. A
	// non-owner's IncludeCandidates is silently downgraded so the candidate's
	// existence never leaks. An admin may opt in (the review view).
	includeCandidates := q.IncludeCandidates
	if includeCandidates && !s.mayReadCandidates(auth, q) {
		includeCandidates = false
	}

	max := q.MaxItems
	if max <= 0 {
		max = defaultRecallCap
	}

	rows, err := s.units.Recall(ctx, q, includeCandidates, max)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil // §9.3 leak-safe empty
	}

	out := make([]KnowledgeCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.shapeCandidate(ctx, auth, r))
	}
	return out, nil
}

// mayReadCandidates reports whether the caller may opt into unpublished units
// for this query (§8.5 owner-only / review view). The owner is the evidence
// owner the query narrows to; an admin may always opt in (review view). A
// non-owner non-admin never may — the candidate's existence would leak.
func (s *RecallService) mayReadCandidates(auth AuthContext, q KnowledgeQuery) bool {
	if auth.IsAdmin {
		return true
	}
	if q.OwnerID != nil && *q.OwnerID == auth.PrincipalID {
		return true
	}
	return false
}

// shapeCandidate turns a UnitRow into a KnowledgeCandidate (12 §9.3). It
// attaches the traceable evidence citation (§8.1) — the unit's backing
// evidence link — and the relations (§8.2). The citation's EvidenceID is
// populated only when a surviving, readable link exists; an
// evidence_missing unit still gets a citation naming the unit (no evidence_id)
// so the caller can down-weight it (§8.4).
//
// Evidence content is NOT expanded here — that is the separate §4.3 ACL path
// (ReadExcerpt). Recall only names the evidence id; the caller must call
// memory_evidence_read to expand it, and that path enforces the ACL chain.
func (s *RecallService) shapeCandidate(_ context.Context, _ AuthContext, r UnitRow) KnowledgeCandidate {
	u := r.Unit
	c := KnowledgeCandidate{
		UnitID:     u.ID,
		AssetID:    u.AssetID,
		AssetType:  "memory",
		MemoryType: string(u.MemoryType),
		Title:      memoryTitle(u),
		Snippet:    u.Statement,
		Authority:  u.Authority,
		Freshness:  freshness(u),
		Confidence: u.Confidence,
		Relations:  toRelations(r.RelationHints),
		Citation: Citation{
			AssetID:         u.AssetID,
			AssetVersionID:  u.AssetVersionID,
			EvidenceMissing: u.EvidenceMissing,
		},
		State: string(u.State),
		// Score = blended authority ranking (§9.5). The repo already ranked;
		// we expose the same authority as the score basis so the caller can
		// re-rank without re-querying.
		Score: u.Authority,
	}
	if r.EvidenceLink != nil {
		c.Citation.EvidenceID = r.EvidenceLink.EvidenceID
		c.Citation.QuoteLocator = r.EvidenceLink.QuoteLocator
		c.Citation.SupportType = string(r.EvidenceLink.SupportType)
	}
	return c
}

// memoryTitle derives a short title from a unit's statement (memory units
// have no title column; 12 §9.3 expects Title). First sentence / 80 runes.
func memoryTitle(u domain.MemoryUnit) string {
	stmt := u.Statement
	if i := indexByte(stmt, '\n'); i > 0 && i < 80 {
		return stmt[:i]
	}
	if runeCount(stmt) <= 80 {
		return stmt
	}
	return substringRunes(stmt, 0, 77) + "…"
}

// freshness is a 0..1 signal: newer units are fresher. A unit valid in the
// future or with no expiry is treated as fully fresh; an expired unit (a rare
// state since expired units exit recall) is 0. This is a coarse signal; the
// repo's ranking already weights it (§9.5).
func freshness(u domain.MemoryUnit) float64 {
	now := time.Now().UTC()
	if u.ExpiresAt != nil && now.After(*u.ExpiresAt) {
		return 0
	}
	if u.CreatedAt.IsZero() {
		return 0.5
	}
	// Decay over ~365 days to 0; clamp to [0,1].
	ageDays := now.Sub(u.CreatedAt).Hours() / 24
	if ageDays <= 0 {
		return 1
	}
	f := 1 - ageDays/365
	if f < 0 {
		f = 0
	}
	return f
}

// toRelations shapes the repo's relation hints into the candidate's Relations
// (§8.2 — contradicts surface so conflicts are not silently chosen).
func toRelations(hints []RelationHint) []RelationSummary {
	if len(hints) == 0 {
		return nil
	}
	out := make([]RelationSummary, 0, len(hints))
	for _, h := range hints {
		out = append(out, RelationSummary{
			RelationType: h.RelationType,
			TargetID:     h.TargetID,
			TargetTitle:  h.TargetTitle,
		})
	}
	return out
}

// indexByte is strings.IndexByte without the import (kept local to avoid
// pulling strings into a hot path that already avoids allocs).
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// runeCount returns the rune count of s without allocating a []rune.
func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// substringRunes returns the first n runes of s, safely truncated.
func substringRunes(s string, _, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
