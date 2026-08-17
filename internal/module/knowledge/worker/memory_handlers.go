// Package worker — Phase 4 memory distill job handlers (design-docs/18 §3.3,
// §5.3, decision D5/D6). These extend the knowledge-worker dispatch table
// with the memory_distill consumer group's job_types:
//
//   - memory_extract    → ExtractService.Extract: load Evidence → Provider →
//                          validate → memory_units(candidate) + links.
//   - memory_dedup       → DedupService (sub-issue C; placeholder for now).
//   - memory_revalidate → revalidate on feedback/expiry (sub-issue C/D).
//
// The handlers are thin: they delegate to the distill.ExtractService and map
// errors to retry dispositions, mirroring the Phase 1/2 handler style. A
// missing/purged Evidence is permanent-dead (nothing to extract, §9.2); a
// malformed Provider output is fail-closed (the service drops it; the Job
// succeeds with Written=0 so the dedupe_key prevents a re-run spamming).
package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/distill"
)

// Memory job_type identifiers (design-docs/18 §3.3 dispatch table).
const (
	JobMemoryExtract    = "memory_extract"
	JobMemoryDedup      = "memory_dedup"
	JobMemoryRevalidate = "memory_revalidate"
)

// MemoryExtractHandler executes the §5.3 extract pipeline for one Evidence.
// The TargetKey carries the evidence_id (the dispatcher set it from the
// `evidence.captured` event payload). The handler is idempotent via the
// knowledge_jobs.dedupe_key — a re-delivered event maps to the same job row
// (worker.ErrJobExists), so it is never re-run.
type MemoryExtractHandler struct {
	// Extract is the distill service wired with the Provider + loader + writer.
	Extract *distill.ExtractService
}

// Run executes the extract pipeline. Evidence-missing → permanent (dead);
// transient Provider failure (upstream unreachable) → transient retry; a
// fail-closed malformed output → succeeded with Written=0 (the Evidence is
// retained; the dedupe_key prevents re-extraction until a manual requeue).
func (h *MemoryExtractHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	if h.Extract == nil {
		return domain.RetryPermanent, fmtErr(JobMemoryExtract, errors.New("memory extract service not wired"))
	}
	if j.TargetKey == "" {
		return domain.RetryPermanent, fmtErr(JobMemoryExtract, errors.New("missing evidence_id in target_key"))
	}
	evidenceID, err := uuidFromTarget(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobMemoryExtract, fmt.Errorf("invalid evidence_id: %w", err))
	}
	outcome, err := h.Extract.Extract(ctx, evidenceID)
	if err != nil {
		// Missing/purged evidence: nothing to extract. Mark permanent-dead so
		// the dispatcher does not retry indefinitely (§9.2).
		if errors.Is(err, distill.ErrEvidenceMissing) {
			return domain.RetryPermanent, fmtErr(JobMemoryExtract, err)
		}
		// Missing memory asset: a config/asset-resolution gap. Permanent so the
		// operator fixes it rather than the worker spinning.
		if errors.Is(err, distill.ErrMemoryAssetNotResolved) {
			return domain.RetryPermanent, fmtErr(JobMemoryExtract, err)
		}
		// Transient (upstream unreachable, DB blip): retry.
		return domain.RetryTransient, fmtErr(JobMemoryExtract, err)
	}
	_ = outcome // Written count; observability hook for the runner log.
	return domain.RetryClass(""), nil
}

// fmtErr lives in runner.go (same package); memory handlers reuse it so the
// error_code column carries a stable job_type prefix.
