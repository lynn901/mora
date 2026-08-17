// Package evidence — deletion propagation (design-docs/18 §9.2, decision D3).
//
// This file owns the orchestration that turns an evidence expiry / explicit
// delete / source_asset revoke into the full §9.2 cascade:
//
//	Evidence 到期 / 显式删除
//	  → state=active → pending_purge（先停止展开，原文仍可审计）
//	  → purge_after 到期 → state=purged：擦除 encrypted_content / storage_key
//	      （+ MinIO 对象删除），保留 id / content_hash / redacted_excerpt / 审计元数据
//	  → 级联：memory_evidence_links 该 evidence 的链接
//	      → memory_units.evidence_missing=true（若无其他独立证据）
//	      → 该 unit 退出高权威召回（authority 降权，不删 statement）
//	  → 级联投影：FTS 行删除 / Qdrant point 删除 / 摘要缓存失效
//	  → 审计：evidence.purged 事件，external_call 审计只保留不可逆摘要与 ID
//	source_asset 删除 → 该 asset 引用的 Evidence 标 evidence_missing（§4.3），
//	  不级联删 Evidence（source_asset_id 无 FK）。
//
// 删除传播路径与状态**先于发布流程实现**（12 §12.2 删除矩阵「删除传播路径和状态必须
// 先于 Phase 4 实现」），是 D（人工发布）/ E（召回）发布态部分的前置门禁。
//
// The service is port-driven: it composes the existing EvidenceRepo /
// MemoryUnitRepo / EvidenceLinkRepo / ObjectStore with two ports owned here —
// ProjectionInvalidator and AuditRecorder — so the FTS/Qdrant/summary delete
// and the audit row are wired by the caller (the distill/recall/publish
// sub-issues land those adapters; until then a NoopProjectionInvalidator keeps
// the cascade total but projection-safe). This keeps the propagation path
// total and testable independent of which projection backends have shipped.
package evidence

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// PurgeOutcome is the per-evidence result of PurgeEvidence, surfaced so a
// reaper / caller can record what cascaded (audit + metrics) without re-reading.
type PurgeOutcome struct {
	EvidenceID     uuid.UUID
	ContentHash    string
	WasLargeObject bool // StorageKey was set → MinIO object delete attempted
	// Units flagged evidence_missing by this purge (only those whose this
	// evidence was the last independent support — §9.2 "若无其他独立证据").
	UnitsFlagged []uuid.UUID
}

// ProjectionInvalidator erases the downstream projections of a memory unit
// when its backing evidence is purged (§9.2 级联投影：FTS 行删除 / Qdrant point
// 删除 / 摘要缓存失效). The memory-unit projections are built at publish
// (§5.3 / §6.2 asset_projections fts/vector/summary); invalidating them here
// means a recall after a purge no longer surfaces content whose proof is gone.
//
// The port is coarse-grained on purpose (per-unit, not per-projection-kind):
// the caller does not need to know which backends back a unit — it asks the
// invalidator to drop everything tied to that unit. Implementations may no-op
// a backend that has no projection for the unit (idempotent).
type ProjectionInvalidator interface {
	// InvalidateUnitProjections removes all FTS rows / Qdrant points / summary
	// cache entries tied to the unit. It is idempotent: a unit with no
	// projection (candidate, never published, or already invalidated) is a
	// no-op. A backend failure is surfaced so the reaper can retry the whole
	// purge — projections and the DB erase must stay consistent (§9.2 逐级传播).
	InvalidateUnitProjections(ctx context.Context, unitID uuid.UUID) error
}

// AuditRecorder appends the §9.4 audit events the propagation path emits.
// Decoupled from platform/audit so the propagation service has no import cycle
// into platform (platform/audit already imports domain; this keeps the evidence
// package leaf-level). The recorder is best-effort relative to the purge: an
// audit write failure must not un-purge content — it surfaces as an error the
// reaper may retry, but the erase + cascade are already committed.
type AuditRecorder interface {
	// RecordEvidencePurged appends the evidence.purged audit row (§9.4):
	// evidence_id + content_hash + purged_at, no original content.
	RecordEvidencePurged(ctx context.Context, e domain.MemoryEvidence, purgedAt time.Time) error
}

// NoopProjectionInvalidator is the default ProjectionInvalidator when no
// projection backend is wired yet (pre-D/E). It keeps the cascade total from
// the propagation service's view — a published unit whose evidence is purged
// still gets evidence_missing flagged; the projection delete becomes a real
// call once the distill/publish path lands the FTS/Qdrant/summary adapters.
type NoopProjectionInvalidator struct{}

// InvalidateUnitProjections is a no-op (the unit has no wired projections yet).
func (NoopProjectionInvalidator) InvalidateUnitProjections(ctx context.Context, unitID uuid.UUID) error {
	return nil
}

// PropagationConfig wires the ports the service composes.
type PropagationConfig struct {
	Evidence    EvidenceRepo
	Units       MemoryUnitRepo
	Links       EvidenceLinkRepo
	Objects     ObjectStore // MinIO large-object delete; nil-safe via the objstore adapter
	Projections ProjectionInvalidator
	Audit       AuditRecorder
	Now         func() time.Time // injectable clock for deterministic tests
}

// PropagationService orchestrates the §9.2 deletion cascade.
type PropagationService struct {
	cfg PropagationConfig
}

// NewPropagationService builds a PropagationService. Projections defaults to
// NoopProjectionInvalidator when nil so the cascade is total even before the
// distill/publish projection adapters land. Now defaults to time.Now.
func NewPropagationService(cfg PropagationConfig) *PropagationService {
	if cfg.Projections == nil {
		cfg.Projections = NoopProjectionInvalidator{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &PropagationService{cfg: cfg}
}

// MarkPendingPurge flips an active evidence to pending_purge (D3 expiry). This
// is the first half of the lifecycle: content stays auditable, expansion stops
// (§9.2 "先停止展开，原文仍可审计"). Idempotent — a pending_purge/purged row
// is a no-op, so the retention reaper can fire this on every due row without
// racing itself. A missing/deleted row returns ErrEvidenceNotFound so the
// reaper can't infer the existence of a purged row (§9.3).
func (s *PropagationService) MarkPendingPurge(ctx context.Context, id uuid.UUID) error {
	return s.cfg.Evidence.MarkPendingPurge(ctx, id)
}

// PurgeEvidence erases an evidence's content and cascades per §9.2. It is the
// second half of the lifecycle (pending_purge/active → purged). It is
// idempotent on the content side: re-purging a row whose content is already
// erased still re-runs the cascade + audit (a no-content re-purge is safe; the
// reaper treats it as "nothing left to erase, re-confirm cascade"). A missing/
// already-purged-and-deleted row returns ErrEvidenceNotFound (§9.3).
//
// Order matters (§9.2 逐级传播):
//  1. Load the row to capture content_hash + storage_key before erasing (the
//     hash is the deletion proof retained post-purge, §2.1 不变量).
//  2. Erase content: repo.Purge nulls encrypted_content/storage_key/key_version
//     and sets state=purged + purged_at (retains id/hash/excerpt/audit).
//  3. Delete the MinIO object for large-object evidence (best-effort: a missing
//     object is not an error so a re-purge is idempotent).
//  4. Cascade to units: for each unit linked to this evidence, if this was its
//     last independent support → MarkEvidenceMissing; regardless, invalidate
//     that unit's projections so recall stops surfacing purged-backed content.
//  5. Audit evidence.purged (§9.4): evidence_id + content_hash + purged_at.
//
// A step-4 projection failure after the DB erase is surfaced: the reaper
// retries the whole PurgeEvidence, which re-loads a now-purged row (content
// already null), re-deletes the (now-missing) MinIO object, re-runs the
// cascade + audit. Idempotency at every step keeps the retry loss-free.
func (s *PropagationService) PurgeEvidence(ctx context.Context, id uuid.UUID) (PurgeOutcome, error) {
	e, err := s.cfg.Evidence.Get(ctx, id)
	if err != nil {
		return PurgeOutcome{}, err
	}

	// 1. DB erase + state flip (idempotent: a purged row is a no-op here).
	if err := s.cfg.Evidence.Purge(ctx, id); err != nil {
		return PurgeOutcome{}, err
	}

	out := PurgeOutcome{
		EvidenceID:  e.ID,
		ContentHash: e.ContentHash,
	}

	// 2. Large-object delete (MinIO). A StorageKey that's already empty (a
	// re-purge, or an inline-encrypted row) skips the object call. The
	// objstore adapter treats a missing object as success (idempotent).
	if e.StorageKey != "" {
		out.WasLargeObject = true
		// Best-effort: do not let an object-store blip un-commit the DB
		// erase. The reaper re-runs PurgeEvidence; a second pass finds
		// StorageKey empty on the row and skips here. We still surface the
		// error so the reaper can retry the object delete before trusting the
		// purge complete.
		if err := s.cfg.Objects.Delete(ctx, e.StorageKey); err != nil {
			return out, err
		}
	}

	// 3. Cascade to linked units (§9.2 级联).
	links, err := s.cfg.Links.ListForEvidence(ctx, id)
	if err != nil {
		return out, err
	}
	seen := make(map[uuid.UUID]struct{}, len(links))
	for _, l := range links {
		if _, dup := seen[l.MemoryUnitID]; dup {
			continue
		}
		seen[l.MemoryUnitID] = struct{}{}

		// "若无其他独立证据" → evidence_missing only when this was the last
		// independent support. CountAvailableEvidence joins through the
		// evidence row excluding this one and dropping purged/deleted rows, so
		// an already-purged sibling does not keep a unit falsely "supported".
		remaining, err := s.cfg.Links.CountAvailableEvidence(ctx, l.MemoryUnitID, id)
		if err != nil {
			return out, err
		}
		if remaining == 0 {
			if err := s.cfg.Units.MarkEvidenceMissing(ctx, l.MemoryUnitID); err != nil {
				return out, err
			}
			out.UnitsFlagged = append(out.UnitsFlagged, l.MemoryUnitID)
		}

		// Regardless of evidence_missing, the unit's projections must drop:
		// a unit with one purged evidence among several still lost a proof,
		// and the purged evidence's specific projection contribution is gone.
		// The invalidator is idempotent per-unit.
		if err := s.cfg.Projections.InvalidateUnitProjections(ctx, l.MemoryUnitID); err != nil {
			return out, err
		}
	}

	// 4. Audit evidence.purged (§9.4). Best-effort relative to content (the
	// erase is committed) but surfaced so the reaper retries the audit row.
	purgedAt := s.cfg.Now()
	if s.cfg.Audit != nil {
		if err := s.cfg.Audit.RecordEvidencePurged(ctx, e, purgedAt); err != nil {
			return out, err
		}
	}

	return out, nil
}

// RevokeSourceAsset handles the §9.2 "撤权" path: a source_asset deletion marks
// every evidence referencing it evidence_missing (so expansions fail closed,
// §4.3 "来源删除/不可定位 → 原文默认不可展开"), but does NOT cascade-delete
// the Evidence rows themselves (source_asset_id carries no FK by design, §2.1
// 不变量). The evidence content stays for audit until its own retention expiry
// flips it through pending_purge → purged.
//
// Because a source_asset revoke is about ACL (can't expand) not lifecycle, it
// does not erase content and does not emit evidence.purged. It may still
// invalidate the linked units' projections if the revoke left a unit with no
// expandable evidence — that decision uses the same CountAvailableEvidence
// read (which drops purged/deleted evidence) so a unit whose only evidence is
// now source-revoked-and-thus-unexpandable is treated as missing. (A revoked
// source_asset does not set evidence.state — the row stays active; only
// expansion is gated by §4.3's source_asset ACL check. The evidence_missing
// flag is the recall-side signal that the proof is no longer trustworthy.)
func (s *PropagationService) RevokeSourceAsset(ctx context.Context, sourceAssetID uuid.UUID) (PurgeOutcome, error) {
	out := PurgeOutcome{}
	ev, err := s.cfg.Evidence.ListBySourceAsset(ctx, sourceAssetID)
	if err != nil {
		return out, err
	}
	for _, e := range ev {
		links, err := s.cfg.Links.ListForEvidence(ctx, e.ID)
		if err != nil {
			return out, err
		}
		seen := make(map[uuid.UUID]struct{}, len(links))
		for _, l := range links {
			if _, dup := seen[l.MemoryUnitID]; dup {
				continue
			}
			seen[l.MemoryUnitID] = struct{}{}
			// Treat the source-revoked evidence as "unavailable" for the count
			// by excluding it — same read the purge path uses. If the unit has
			// no other expandable evidence, flag it.
			remaining, err := s.cfg.Links.CountAvailableEvidence(ctx, l.MemoryUnitID, e.ID)
			if err != nil {
				return out, err
			}
			if remaining == 0 {
				if ferr := s.cfg.Units.MarkEvidenceMissing(ctx, l.MemoryUnitID); ferr != nil {
					return out, ferr
				}
				out.UnitsFlagged = append(out.UnitsFlagged, l.MemoryUnitID)
			}
			if perr := s.cfg.Projections.InvalidateUnitProjections(ctx, l.MemoryUnitID); perr != nil {
				return out, perr
			}
		}
	}
	return out, nil
}
