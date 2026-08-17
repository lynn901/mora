// Package auditbridge adapts the append-only audit log (07 §5) to the
// evidence.AuditRecorder port (design-docs/18 §9.4).
//
// It lives in its own leaf package — NOT in platform/audit — to break the
// import cycle that arose when the bridge was originally placed in package
// audit: platform/audit imported module/memory/evidence (for the
// AuditRecorder port) while evidence imported platform/audit (for the Logger
// the capture/inbox services gate on). A bridge that depends on both sides
// must sit in a leaf that neither side imports back; platform/audit stays a
// pure leaf (Repo + Logger, no module dependency) and evidence stays a leaf
// over platform/audit only.
//
// The wiring layer (cmd/mora-api, cmd/knowledge-worker) composes this adapter:
// it owns a *audit.Repo and a *audit.Logger, hands the Logger to the
// evidence/inbox services via WithAuthz, and hands NewEvidenceAuditRecorder
// to the propagation service as its AuditRecorder. One audit implementation,
// two adaptation shapes (Logger for the inline capture path, Recorder for the
// §9.4 erase audit) — both backed by the same append-only table.
package auditbridge

import (
	"context"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
	"github.com/lynn901/mora/internal/platform/audit"
)

// EvidenceAuditRecorder bridges audit.Repo → evidence.AuditRecorder.
type EvidenceAuditRecorder struct{ repo audit.Repo }

// NewEvidenceAuditRecorder wraps an audit.Repo for the §9.4 evidence events.
func NewEvidenceAuditRecorder(repo audit.Repo) *EvidenceAuditRecorder {
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
