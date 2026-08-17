// Package audit — evidence audit bridge (design-docs/18 §9.4).
//
// EvidenceAuditRecorder adapts the append-only audit log (07 §5) to the
// evidence.AuditRecorder port (internal/module/memory/evidence). The deletion
// propagation path (§9.2) emits the evidence.purged audit row through this
// bridge so the erase is auditable as evidence_id + content_hash + purged_at,
// with no original content (12 §8.4 审计记录只保留不可逆摘要与 ID).
//
// The bridge lives in platform/audit (not in the evidence package) because it
// depends on the audit Repo port + domain.AuditLog shape; the evidence package
// stays a leaf with no platform dependency, so the propagation service composes
// this adapter from the wiring layer (cmd/mora-api, cmd/knowledge-worker).
package audit

import (
	"context"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// EvidenceAuditRecorder bridges audit.Repo → evidence.AuditRecorder.
type EvidenceAuditRecorder struct{ repo Repo }

// NewEvidenceAuditRecorder wraps an audit.Repo for the §9.4 evidence events.
func NewEvidenceAuditRecorder(repo Repo) *EvidenceAuditRecorder {
	return &EvidenceAuditRecorder{repo: repo}
}

// RecordEvidencePurged appends the evidence.purged audit row (§9.4): action
// evidence.purged, target_type=evidence, target_id=evidence_id, detail carries
// content_hash + purged_at — no original content, no storage_key (the deletion
// proof is the retained hash, §2.1 不变量). Best-effort relative to the content
// erase: a failure here surfaces so the reaper may retry, but the row is
// append-only so a retry only ever adds the one missing row (idempotent intent,
// not idempotent write — the propagation service treats an audit failure as
// retryable, not fatal to the committed erase).
func (r *EvidenceAuditRecorder) RecordEvidencePurged(ctx context.Context, e domain.MemoryEvidence, purgedAt time.Time) error {
	evID := e.ID
	log := &domain.AuditLog{
		ActorType:  string(domain.OwnerServiceAccount),
		Action:     "evidence.purged",
		TargetType: string(domain.TargetEvidence),
		TargetID:   &evID,
		Detail: map[string]any{
			"evidence_id":  e.ID.String(),
			"workspace_id": e.WorkspaceID.String(),
			"content_hash": e.ContentHash,
			"purged_at":    purgedAt.UTC().Format(time.RFC3339),
		},
		CreatedAt: time.Now().UTC(),
	}
	return r.repo.Append(ctx, log)
}

// Compile-time check: EvidenceAuditRecorder satisfies evidence.AuditRecorder.
var _ evidence.AuditRecorder = (*EvidenceAuditRecorder)(nil)
