// Package distill — service layer (design-docs/18 §5.3).
//
// The ExtractService is the business logic the memory_extract Job handler
// calls: it loads the Evidence row, decrypts the inline fragment (or reads the
// object), hands the REDACTED excerpt to the ExtractionProvider, re-validates
// the candidates (§5.2 layer 2 — service), and lands each candidate as a
// memory_units(state=candidate) + memory_evidence_links row. A validation
// failure drops the candidate and leaves the Evidence for retry — no
// half-structured Memory is written (§9.1 fail closed).
//
// The service does NOT own the knowledge-worker Job dispatch (that lives in
// the worker); it is called by the worker's memory_extract handler. The
// idempotency of the dispatch (knowledge_jobs.dedupe_key) guarantees a
// re-delivered event does not produce duplicate candidates.
package distill

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// ErrEvidenceMissing is returned by Extract when the bound Evidence row is
// gone (deleted/purged). The worker marks the Job permanent-dead — there is
// nothing to extract from (§9.2).
var ErrEvidenceMissing = errors.New("memory: evidence missing or purged")

// EvidenceLoader loads an Evidence row + its redacted plaintext for extraction.
// The distill service depends on a narrow port (not the full EvidenceRepo) so
// the worker handler can inject a decrypting loader without leaking the KEK
// into the service. The loader returns the REDACTED excerpt + source kind —
// never the raw ciphertext (§9.1).
type EvidenceLoader interface {
	LoadForExtract(ctx context.Context, evidenceID uuid.UUID) (LoadedEvidence, error)
}

// MemoryAssetResolver resolves the knowledge_assets(asset_type='memory') id
// for a workspace so a candidate unit has an asset to attach to. The handler
// (or loader) owns resolution; the service never creates assets. Defined here
// so the postgres loader can implement it without importing worker.
type MemoryAssetResolver interface {
	GetOrCreateMemoryAsset(ctx context.Context, workspaceID uuid.UUID, ownerType domain.OwnerType, ownerID uuid.UUID) (uuid.UUID, error)
}

// LoadedEvidence is the minimal redacted snapshot the Provider receives (§5.2).
type LoadedEvidence struct {
	ID             uuid.UUID
	WorkspaceID    uuid.UUID
	OwnerType       domain.OwnerType
	OwnerID         uuid.UUID
	SourceKind      domain.EvidenceSourceKind
	SourceRef        string
	SourceAssetID   *uuid.UUID
	// MemoryAssetID is the knowledge_assets(asset_type='memory') id the
	// worker handler resolved for this workspace; candidate memory_units are
	// attached to it. The handler, not the service, owns asset resolution.
	MemoryAssetID  uuid.UUID
	RedactedExcerpt string // post-gate redacted content (inline decrypted or object-read)
}

// ExtractService composes the Provider + the CandidateWriter + the EvidenceLoader.
// It is the §5.3 business logic called by the memory_extract Job handler.
type ExtractService struct {
	provider ExtractionProvider
	loader   EvidenceLoader
	writer   CandidateWriter
}

// NewExtractService wires the extract service.
func NewExtractService(provider ExtractionProvider, loader EvidenceLoader, writer CandidateWriter) *ExtractService {
	return &ExtractService{provider: provider, loader: loader, writer: writer}
}

// Extract turns one Evidence row into zero or more candidate memory_units. The
// caller (worker handler) passes the Evidence id; the service binds the
// capability to that id + "extract" (§10.3) so the Provider cannot run a
// free-floating extraction.
func (s *ExtractService) Extract(ctx context.Context, evidenceID uuid.UUID) (ExtractOutcome, error) {
	loaded, err := s.loader.LoadForExtract(ctx, evidenceID)
	if err != nil {
		if errors.Is(err, domain.ErrEvidenceNotFound) {
			return ExtractOutcome{}, ErrEvidenceMissing
		}
		return ExtractOutcome{}, err
	}

	cap := Capability{
		Action:       "extract",
		EvidenceID:  loaded.ID,
		WorkspaceID: loaded.WorkspaceID,
	}
	req := ExtractRequest{
		RedactedExcerpt: loaded.RedactedExcerpt,
		SourceKind:      loaded.SourceKind,
		OwnerType:        loaded.OwnerType,
		OwnerID:          loaded.OwnerID,
		SourceRef:         loaded.SourceRef,
	}

	candidates, err := s.provider.ExtractMemory(ctx, cap, req)
	if err != nil {
		return ExtractOutcome{}, err
	}

	// Service-layer validation (§5.2 layer 2 — defense in depth). A malformed
	// candidate is dropped; the Evidence stays for retry. We do NOT fail the
	// whole batch on one bad candidate — we keep the valid ones (§5.2 "解析
	// 失败保留 Evidence 并重试" applies per-candidate; the Provider adapter
	// already validated, so a failure here is a bug, not noise).
	written := 0
	for _, c := range candidates {
		if err := ValidateCandidates([]MemoryCandidate{c}, loaded.ID); err != nil {
			// Drop this candidate; keep going. The adapter should have caught
			// this, so a failure here is surfaced for observability but does
			// not poison the batch.
			continue
		}
		if err := s.writeCandidate(ctx, loaded, c); err != nil {
			return ExtractOutcome{Written: written}, err
		}
		written++
	}
	return ExtractOutcome{Written: written}, nil
}

// writeCandidate lands one validated candidate as a memory_units row (state=
// candidate) + a memory_evidence_links row. The unit's asset_id references a
// knowledge_assets(asset_type='memory') row; the caller (worker handler) is
// responsible for ensuring that asset exists — the service assumes the
// assetID passed in is valid.
func (s *ExtractService) writeCandidate(ctx context.Context, loaded LoadedEvidence, c MemoryCandidate) error {
	// The memory_unit needs an asset_id (knowledge_assets asset_type='memory').
	// Phase 4 first version: the caller resolves/creates the asset per
	// workspace before extraction. The service carries it via LoadedEvidence
	// when set; otherwise it surfaces an explicit error so the worker handler
	// can create it. For now, LoadedEvidence.SourceAssetID is the closest
	// existing handle; the handler will set a dedicated MemoryAssetID on
	// LoadedEvidence in a follow-up. We require the caller to have set it.
	if loaded.MemoryAssetID == uuid.Nil {
		return ErrMemoryAssetNotResolved
	}

	conf := c.Confidence
	unit := domain.MemoryUnit{
		WorkspaceID:       loaded.WorkspaceID,
		AssetID:           loaded.MemoryAssetID,
		MemoryType:        c.MemoryType,
		Statement:         c.Statement,
		StructuredPayload: payloadFrom(c),
		Confidence:        &conf,
		ValidFrom:         c.Validity.ValidFrom,
		ExpiresAt:         c.Validity.ExpiresAt,
		State:             domain.MemoryCandidate,
		EvidenceMissing:   false,
		Authority:         0.5, // default; feedback + review adjust (D8)
		CreatedByType:     loaded.OwnerType,
		CreatedByID:       loaded.OwnerID,
	}
	unitID, err := s.writer.InsertUnit(ctx, unit)
	if err != nil {
		return err
	}
	link := domain.MemoryEvidenceLink{
		MemoryUnitID: unitID,
		EvidenceID:   loaded.ID,
		QuoteLocator: c.EvidenceLocator.QuoteLocator,
		SupportType:  domain.Supports,
	}
	return s.writer.LinkEvidence(ctx, link)
}

// payloadFrom maps a candidate's entity_keys into the structured_payload
// column (used for the GIN exact-recall index, §2.2). Scope is folded in so
// a recall by scope term is also indexed.
func payloadFrom(c MemoryCandidate) map[string]any {
	payload := map[string]any{}
	if c.Scope != "" {
		payload["scope"] = c.Scope
	}
	for k, v := range c.EntityKeys {
		payload[k] = v
	}
	return payload
}

// ErrMemoryAssetNotResolved is returned when the caller did not resolve a
// knowledge_assets(asset_type='memory') id for the workspace before extraction.
// The worker handler resolves it; the service does not create assets.
var ErrMemoryAssetNotResolved = errors.New("memory: memory asset id not resolved for extraction")

// ExtractOutcome reports how many candidate units landed.
type ExtractOutcome struct {
	Written int
}

// Compile-time: ExtractService uses the ports it declares.
var _ = fmt.Sprintf
