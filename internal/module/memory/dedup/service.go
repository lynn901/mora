// Package dedup implements the Phase 4 dedup/conflict-suggestion pipeline
// (design-docs/18 §6.1, §8.3, decision D7).
//
// The DedupService takes a newly-extracted candidate memory unit and produces
// NON-MERGING suggestions: it structurally filters the workspace's live units
// (same workspace + memory_type + overlapping validity), feeds each neighbor
// pair to the ExtractionProvider.ClassifyRelation, and lands the result as:
//
//   - duplicate/extends → a memory_dedup_suggestions row (state=pending). The
//     reviewer later confirms → memory_units.superseded_by (the superseded
//     unit deprecates). The dedup service NEVER writes superseded_by itself.
//   - contradicts → a knowledge_relations(relation_type='contradicts') row
//     (pending reviewer-facing edge) AND a memory_dedup_suggestions row. The
//     relation row is what recall surfaces (§8.2 — recall returns contradicts
//     Relations, never silently picking one answer); the suggestion row tracks
//     the reviewer workflow. This split keeps contradicts from polluting the
//     suggestion-table semantics (验收门禁: "`contradicts` 正确落
//     `knowledge_relations`，不污染 `memory_dedup_suggestions` 语义").
//   - unrelated → no suggestion row (the pair needs no reviewer attention).
//
// The service NEVER auto-merges (附录 A 不变量 9). It only produces
// suggestions + the contradicts relation edge; every supersede/merge is a
// reviewer decision (§6.1 step 5, §8.3 第 4 条).
//
// The Provider sees only the two redacted statements + scopes — no Evidence
// IDs leak to an upstream model (the adapter binds the capability, §9.1).
package dedup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/distill"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// ErrUnitNotFound is returned by Dedup when the unit being classified does not
// resolve (or is not visible). Indistinguishable from a permission denial so
// existence is never leaked (§9.3).
var ErrUnitNotFound = errors.New("memory: unit not found or not visible for dedup")

// DedupService composes the unit repo (neighbor recall), the Provider
// (ClassifyRelation), the suggestion repo, and the relation writer. It is the
// §6.1 business logic the memory_dedup Job handler calls.
type DedupService struct {
	units     evidence.MemoryUnitRepo
	provider  distill.ExtractionProvider
	suggestions evidence.DedupSuggestionRepo
	relations evidence.KnowledgeRelationWriter
}

// NewDedupService wires the dedup service.
func NewDedupService(units evidence.MemoryUnitRepo, provider distill.ExtractionProvider, suggestions evidence.DedupSuggestionRepo, relations evidence.KnowledgeRelationWriter) *DedupService {
	return &DedupService{units: units, provider: provider, suggestions: suggestions, relations: relations}
}

// DedupOutcome reports how many suggestions + contradicts relations landed.
type DedupOutcome struct {
	Suggestions int
	Contradicts int
}

// Classify runs the §6.1 dedup pipeline for one candidate unit. It is
// idempotent at the suggestion level only insofar as the caller (the worker)
// dedupe_key guards re-runs; the service itself does not dedupe prior pending
// suggestions (a reviewer may reject a stale one and a re-run produces a fresh
// pending row — acceptable, the inbox sorts by created_at).
func (s *DedupService) Classify(ctx context.Context, unitID uuid.UUID) (DedupOutcome, error) {
	unit, err := s.units.Get(ctx, unitID)
	if err != nil {
		if errors.Is(err, domain.ErrMemoryUnitNotFound) {
			return DedupOutcome{}, ErrUnitNotFound
		}
		return DedupOutcome{}, err
	}

	// Step 1: structural filter (§6.1 step 1). Same workspace + memory_type;
	// the repo also restricts to live (candidate/published) units and omits
	// the unit itself. Validity-overlap is checked in-process to keep the
	// repo query simple (a unit's validity window is in-memory data).
	neighbors, err := s.units.ListCandidateNeighbors(ctx, unit.WorkspaceID, unit.MemoryType, unit.ID)
	if err != nil {
		return DedupOutcome{}, err
	}

	out := DedupOutcome{}
	for _, nb := range neighbors {
		if !validityOverlaps(unit, nb) {
			continue
		}
		// Step 2+3: ClassifyRelation (§6.1 steps 2–3). The capability binds
		// the unit's evidence (via the unit id proxy — no evidence id is
		// handed to an upstream model; the adapter terminates capability at
		// the Mora adapter, §9.1) + "classify_relation".
		cap := distill.Capability{
			Action:       "classify_relation",
			EvidenceID:  unit.ID, // capability binding anchor; not leaked upstream
			WorkspaceID: unit.WorkspaceID,
		}
		req := distill.RelationRequest{
			StatementA: unit.Statement,
			ScopeA:     scopeOf(unit),
			StatementB: nb.Statement,
			ScopeB:     scopeOf(nb),
		}
		suggestion, err := s.provider.ClassifyRelation(ctx, cap, req)
		if err != nil {
			// A Provider failure on ONE pair must not abort the whole batch —
			// the remaining neighbors still classify. Skip this pair; the
			// caller's dedupe_key prevents a re-run spamming, so a transient
			// failure leaves the pair un-suggested (acceptable — reviewer can
			// request a re-dedup). Mirrors the extract service's per-candidate
			// drop (distill/service.go).
			continue
		}
		if suggestion.Relation == domain.DedupUnrelated {
			continue // §6.1 step 4: unrelated → no suggestion row
		}
		if err := s.landSuggestion(ctx, unit, nb, suggestion); err != nil {
			return out, err
		}
		out.Suggestions++
		if suggestion.Relation == domain.DedupContradicts {
			out.Contradicts++
		}
	}
	return out, nil
}

// landSuggestion persists one classified pair. duplicate/extends →
// memory_dedup_suggestions only. contradicts → memory_dedup_suggestions +
// knowledge_relations(contradicts). The suggestion row's evidence_ref carries
// the confidence + rationale (non-identity, no original text). The relation
// row records the contradicts edge so recall (§8.2) can surface both sides.
//
// The supersede direction (which unit supersedes which) is NOT decided here —
// only the reviewer confirms it (§6.1 step 5). unit_a/unit_b are ordered by id
// so the same pair always lands in the same row direction (stable, avoids
// duplicate A↔B vs B↔A rows).
func (s *DedupService) landSuggestion(ctx context.Context, a, b domain.MemoryUnit, sug distill.RelationSuggestion) error {
	unitA, unitB := a, b
	if b.ID.String() < a.ID.String() {
		unitA, unitB = b, a
	}
	conf := sug.Confidence
	suggestion := domain.MemoryDedupSuggestion{
		WorkspaceID:    unitA.WorkspaceID,
		UnitAID:        unitA.ID,
		UnitBID:        unitB.ID,
		SuggestionType: sug.Relation,
		Origin:         domain.DedupOriginGenerated,
		Confidence:     &conf,
		EvidenceRef: map[string]any{
			"rationale":  sug.Rationale,
			"confidence": sug.Confidence,
		},
		State: domain.DedupPending,
	}
	if _, err := s.suggestions.Insert(ctx, suggestion); err != nil {
		return fmt.Errorf("memory dedup: insert suggestion: %w", err)
	}

	if sug.Relation == domain.DedupContradicts {
		// contradicts → knowledge_relations(relation_type='contradicts') (§8.3).
		// The edge is directed from the newer unit to the older one; recall
		// surfaces both via the idx_relations_from/to indexes. origin=
		// generated (the Provider suggested it); a reviewer's reject of the
		// suggestion row leaves the relation row in place for recall — the
		// relation is a fact (two statements conflict), the suggestion is the
		// workflow state. This is the 验收门禁 split: contradicts lands in
		// knowledge_relations, the suggestion row tracks disposition.
		//
		// Per-unit granularity (021): unitA/unitB may share one
		// knowledge_assets(memory) row (intra-asset — same evidence source's
		// sibling units). The relaxed CHECK permits from_asset_id=to_asset_id
		// for contradicts; FromUnitID/ToUnitID disambiguate which two units the
		// edge joins, so recall (§8.2) reads the unit ids directly instead of
		// joining memory_units per query. Cross-asset contradicts is not a
		// memory-dedup case (dedup only pairs units within a workspace + type).
		unitAID, unitBID := unitA.ID, unitB.ID
		rel := domain.KnowledgeRelation{
			WorkspaceID:   unitA.WorkspaceID,
			FromAssetID:   unitA.AssetID,
			RelationType:  domain.RelationContradicts,
			ToAssetID:     unitB.AssetID,
			FromUnitID:    &unitAID,
			ToUnitID:      &unitBID,
			Origin:        domain.RelationOriginGenerated,
			Confidence:    &conf,
			CreatedByType: unitA.CreatedByType.ToSubjectType(),
			CreatedByID:   unitA.CreatedByID,
		}
		if _, err := s.relations.InsertRelation(ctx, rel); err != nil {
			return fmt.Errorf("memory dedup: insert contradicts relation: %w", err)
		}
	}
	return nil
}

// validityOverlaps returns true if two units' validity windows overlap (§6.1
// step 1). A nil valid_from means open-ended start (-∞); a nil expires_at
// means open-ended end (+∞). overlap = max(start) < min(end). Two fully
// open-ended units always overlap. A unit whose expires_at is before the
// other's valid_from does not overlap.
func validityOverlaps(a, b domain.MemoryUnit) bool {
	start := latestTime(a.ValidFrom, b.ValidFrom) // -∞ if either nil
	end := earliestTime(a.ExpiresAt, b.ExpiresAt) // +∞ if either nil
	if end == nil {
		return true // at least one is open-ended → overlap
	}
	if start == nil {
		return true // both start at -∞; end is finite → overlap
	}
	return start.Before(*end) || start.Equal(*end)
}

// latestTime returns the later of two *time.Time, or nil if either is nil
// (treat nil as -∞ for the start bound).
func latestTime(a, b *time.Time) *time.Time {
	if a == nil || b == nil {
		return nil
	}
	if a.After(*b) {
		return a
	}
	return b
}

// earliestTime returns the earlier of two *time.Time, or nil if either is nil
// (treat nil as +∞ for the end bound).
func earliestTime(a, b *time.Time) *time.Time {
	if a == nil || b == nil {
		return nil
	}
	if a.Before(*b) {
		return a
	}
	return b
}

// scopeOf extracts the scope from a unit's structured_payload (folded in by
// the distill service's payloadFrom, §5.2). Empty when the candidate carried
// no scope.
func scopeOf(u domain.MemoryUnit) string {
	if u.StructuredPayload == nil {
		return ""
	}
	if v, ok := u.StructuredPayload["scope"].(string); ok {
		return v
	}
	return ""
}
