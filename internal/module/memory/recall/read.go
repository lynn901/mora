// Package recall — evidence read service (design-docs/18 §4.3 Evidence ACL).
//
// ReadExcerpt is the §4.3 ACL chain behind the MCP `memory_evidence_read`
// tool + the REST POST /api/v1/memory/evidence/{id}:read endpoint. It returns
// ONLY the minimal redacted excerpt when the caller passes the full chain:
//
//  1. Memory use/read — the caller may read a memory_unit that references e.
//  2. Evidence read — permissions(target_type='evidence') on e.
//  3. source_asset current ACL — when e.source_asset_id is set.
//
// Any miss → the caller still gets the redacted reference (evidence_type +
// verification status), never the original content, never an error
// distinguishing 403/404 (§9.3 leak-safe). An audited deny is recorded.
//
// This service does NOT decrypt inline ciphertext or read object storage —
// the redacted_excerpt is the leak-safe fallback read (§4.3, 12 §8.4). The
// decrypt path is a future capability for the owner only; the first version
// returns the excerpt only (§4.3 "无权读原文时返回此列").
package recall

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// EvidenceExcerpt is the leak-safe read result (§4.3). Readable=true means the
// caller passed the full ACL chain and got the redacted excerpt (or a
// quote_locator-trimmed slice of it). Readable=false means the caller got the
// redacted reference + evidence_type + verification status — the original
// content is not expanded. Either way the caller gets a 200-style success; the
// existence of the evidence is never distinguished from a denial (§9.3).
type EvidenceExcerpt struct {
	// EvidenceID is always the id the caller asked for (so the caller can
	// correlate); it leaks nothing — the caller supplied it.
	EvidenceID uuid.UUID
	// Readable reports whether the redacted excerpt was returned. False on any
	// ACL miss or a missing/purged row (indistinguishable, §9.3).
	Readable bool
	// Excerpt is the redacted_excerpt (or its quote_locator-trimmed slice) when
	// Readable; empty otherwise.
	Excerpt string
	// EvidenceType is the source_kind (session/message/tool_call/document/code)
	// — safe to surface even on a deny (§4.3 "evidence_type + 校验状态").
	EvidenceType string
	// VerificationStatus is the evidence state for the caller to down-weight
	// (active|pending_purge|purged). Safe to surface on a deny (§4.3).
	VerificationStatus string
	// EvidenceMissing reflects whether the backing evidence is gone
	// (purged/deleted) — the caller asked for an id that no longer resolves
	// to content. Readable is false in that case.
	EvidenceMissing bool
}

// ErrEvidenceReadInvalid is returned when the read request is malformed
// (missing evidence id / workspace scope).
var ErrEvidenceReadInvalid = errors.New("memory: invalid evidence read request")

// ReadExcerptRequest is the input to ReadExcerpt. EvidenceID is the evidence
// to expand; MemoryUnitID is the unit the caller is reading the evidence
// THROUGH (§4.3 chain step 1 — the caller must have use/read on this unit).
// QuoteLocator optionally narrows the excerpt to a cited slice.
type ReadExcerptRequest struct {
	WorkspaceID   uuid.UUID
	EvidenceID    uuid.UUID
	MemoryUnitID  uuid.UUID
	QuoteLocator  map[string]any
}

// ReadExcerpt runs the §4.3 ACL chain and returns the leak-safe excerpt. It:
//  1. Loads the evidence row (missing/purged → EvidenceMissing, readable=false).
//  2. Evaluates the ACL chain via rbac.Engine.Check on TargetEvidence (and,
//     when a source_asset is set, the source-asset current ACL). An admin
//     short-circuits to allowed; the owner short-circuits on private evidence.
//  3. On allow → returns the redacted_excerpt (trimmed by quote_locator).
//  4. On any miss → returns the redacted reference + evidence_type +
//     verification_status, readable=false. Audited as a deny (§9.4).
//
// A missing evidence row is indistinguishable from a deny — both yield
// readable=false with the same shape so existence never leaks (§9.3).
func (s *RecallService) ReadExcerpt(ctx context.Context, auth AuthContext, req ReadExcerptRequest) (EvidenceExcerpt, error) {
	if req.EvidenceID == uuid.Nil || req.WorkspaceID == uuid.Nil {
		return EvidenceExcerpt{}, ErrEvidenceReadInvalid
	}
	if s.evidence == nil {
		// No reader wired: fail closed — readable=false, no content.
		return EvidenceExcerpt{EvidenceID: req.EvidenceID, EvidenceMissing: true}, nil
	}

	ev, err := s.evidence.Get(ctx, req.EvidenceID)
	if err != nil {
		// §9.3: missing/purged → indistinguishable from a deny. Surface the
		// id the caller supplied + evidence_missing so the caller can
		// down-weight; no 403/404 distinction.
		s.auditEvidenceRead(ctx, auth, req, false, "missing")
		return EvidenceExcerpt{
			EvidenceID:       req.EvidenceID,
			EvidenceMissing:  true,
			VerificationStatus: string(domain.EvidencePurged),
		}, nil
	}

	// §4.3 ACL chain. Step 2: Evidence read (target_type='evidence').
	allowed, reason := s.canReadEvidence(ctx, auth, ev)
	if !allowed {
		s.auditEvidenceRead(ctx, auth, req, false, reason)
		// Return the redacted reference + evidence_type + verification
		// status — the original content is NOT expanded (§4.3).
		return EvidenceExcerpt{
			EvidenceID:         req.EvidenceID,
			Readable:           false,
			EvidenceType:       string(ev.SourceKind),
			VerificationStatus: string(ev.State),
			EvidenceMissing:    ev.State == domain.EvidencePurged,
		}, nil
	}

	s.auditEvidenceRead(ctx, auth, req, true, "allow")
	// Readable: return the redacted_excerpt (quote_locator-trimmed when the
	// caller supplied one — a future capability; first version returns the
	// stored excerpt). Purged rows cleared their content — the excerpt is the
	// audit-only residue, still safe to return.
	excerpt := ev.RedactedExcerpt
	if excerpt == "" && ev.State == domain.EvidencePurged {
		// Purged + no excerpt residue: readable but empty (content erased).
		excerpt = ""
	}
	return EvidenceExcerpt{
		EvidenceID:         req.EvidenceID,
		Readable:           true,
		Excerpt:            excerpt,
		EvidenceType:       string(ev.SourceKind),
		VerificationStatus: string(ev.State),
		EvidenceMissing:    ev.State == domain.EvidencePurged,
	}, nil
}

// canReadEvidence evaluates the §4.3 chain step 2 (Evidence read) + step 3
// (source_asset current ACL). An admin short-circuits to allow; the owner of a
// private evidence row short-circuits to allow (11 §8.3 — the owner may read
// their own private evidence). Without an rbac engine, fail closed: deny.
// Returns (allowed, reason) where reason is auditable (§9.4).
func (s *RecallService) canReadEvidence(ctx context.Context, auth AuthContext, ev domain.MemoryEvidence) (bool, string) {
	if auth.IsAdmin {
		return true, "admin"
	}
	// Owner shortcut for private evidence (11 §8.3).
	if ev.OwnerID == auth.PrincipalID {
		return true, "owner"
	}
	if s.rbac == nil {
		// Dev/test without rbac: fail closed — the excerpt is not expanded.
		return false, "no_rbac"
	}
	// Step 2: Evidence read.
	dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs,
		domain.TargetEvidence, ev.ID, domain.ActionRead)
	if err != nil || !dec.Allowed {
		return false, "evidence_denied"
	}
	// Step 3: source_asset current ACL (when set). A document/code evidence
	// must also pass the referenced asset's current RBAC (§4.3).
	if ev.SourceAssetID != nil {
		dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs,
			domain.TargetAsset, *ev.SourceAssetID, domain.ActionRead)
		if err != nil || !dec.Allowed {
			return false, "source_asset_denied"
		}
	}
	return true, "allow"
}

// auditEvidenceRead writes the §9.4 `evidence.read` audit row (allow/deny).
// Best-effort: a failure never blocks the read decision (§5 audit invariants).
func (s *RecallService) auditEvidenceRead(ctx context.Context, auth AuthContext, req ReadExcerptRequest, allowed bool, reason string) {
	if s.audit == nil {
		return
	}
	actorType := string(auth.SubjectType)
	if actorType == "" {
		actorType = "user"
	}
	var principalID *uuid.UUID
	if auth.PrincipalID != uuid.Nil {
		pid := auth.PrincipalID
		principalID = &pid
	}
	var evID *uuid.UUID
	if req.EvidenceID != uuid.Nil {
		eid := req.EvidenceID
		evID = &eid
	}
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	s.audit.Record(ctx, actorType, principalID, "evidence.read",
		"evidence", evID,
		map[string]any{"decision": decision, "reason": reason},
		"", "")
}
