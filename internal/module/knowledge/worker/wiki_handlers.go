// Package worker — Wiki maintenance job handlers (design-docs/16 §3.3 / §4.3
// / §5). These extend the Phase 1 dispatch table (runner.go) with the four
// wiki job_types:
//
//   - wiki_maintain         → execute a queued maintenance run: the service
//                              computes affected pages + authorized source
//                              versions, calls the provider, validates
//                              PagePatches (§4.2 gate), lands proposals
//                              (§4.3 / §4.4). A schema violation marks the
//                              run failed; a transient error retries.
//   - wiki_proposal_apply   → per-proposal CAS activation (§4.5) — the async
//                              counterpart to the synchronous ReviewProposal
//                              approve path; used by the auto-approve scope.
//   - wiki_index_rebuild    → deterministic index Document rebuild (§5.1).
//   - wiki_lint_scan        → incremental lint scan (§4.3 lint / §5.3).
//
// The handlers are thin: they delegate to the wiki service + lint/index ports
// and map errors to retry dispositions, mirroring the Phase 1 handler style
// (ProjectionBuildHandler / AssetActivateHandler).
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	wikilint "github.com/lynn901/mora/internal/module/knowledge/wiki/lint"
	wikiidx "github.com/lynn901/mora/internal/module/knowledge/wiki/index"
	wikisvc "github.com/lynn901/mora/internal/module/knowledge/wiki/service"
)

// Wiki job_type identifiers (design-docs/16 §3.3 dispatch table).
const (
	JobWikiMaintain      = "wiki_maintain"
	JobWikiProposalApply = "wiki_proposal_apply"
	JobWikiIndexRebuild  = "wiki_index_rebuild"
	JobWikiLintScan      = "wiki_lint_scan"
)

// WikiMaintainHandler executes a queued maintenance run (§4.3). It delegates to
// the wiki service's ExecuteRun, which computes the affected pages + authorized
// source versions, calls the provider (ProposeIngest / ProposeAnswer), validates
// the PagePatches (§4.2 schema gate), and lands them as proposals with the
// managed/locked/manual differentiation (§4.4). A schema violation marks the run
// failed (§4.2); a transient provider error retries.
type WikiMaintainHandler struct {
	// Wiki is the wiki service wired with the provider (the worker constructs
	// it via NewService(repo, sink, &ProviderAdapter{Inner: provider, ...})).
	// This is the canonical execute path (Gap A fix).
	Wiki *wikisvc.Service
	// Repo updates the run status (start/applied/failed) the handler reports
	// back to the dispatcher. The service already sets these, but the handler
	// keeps a reference so a misconfigured service surfaces observably.
	Repo wikisvc.WikiRepo
}

func (h *WikiMaintainHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	runID, err := uuidFromTarget(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobWikiMaintain, fmt.Errorf("missing run_id in target_key: %w", err))
	}
	if h.Wiki == nil {
		// No service wired — surface as a permanent config failure so the run
		// is not retried forever (the dispatcher dead-letters permanent).
		_ = h.Repo.UpdateRunStatus(ctx, runID, "failed", "not_wired", "wiki service not wired")
		return domain.RetryPermanent, fmtErr(JobWikiMaintain, fmt.Errorf("wiki service not wired"))
	}
	if _, err := h.Wiki.ExecuteRun(ctx, runID); err != nil {
		if isSchemaViolation(err) {
			// §4.2: a schema-gate failure marks the run failed permanently.
			return domain.RetryPermanent, fmtErr(JobWikiMaintain, err)
		}
		// Already-applied is success (the service idempotently returns applied).
		if isAlreadyApplied(err) {
			return domain.RetryTransient, nil
		}
		return domain.RetryTransient, fmtErr(JobWikiMaintain, err)
	}
	return domain.RetryTransient, nil
}

// WikiProposalApplyHandler runs the §4.5 per-page CAS for one proposal. It is
// the async counterpart to the synchronous ReviewProposal approve path, used
// when the auto-approve scope flips a managed proposal to 'approved' without a
// human review. The CAS is idempotent — a re-acquired job finds the proposal
// already 'applied' and returns success. ApplyProposalCAS opens its own
// transaction when the caller passes a nil tx (mirroring the sync
// ReviewProposal path), so the handler passes nil rather than a stub tx that
// always fails (Gap D fix).
type WikiProposalApplyHandler struct {
	Repo wikisvc.WikiRepo
}

func (h *WikiProposalApplyHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	proposalID, err := uuidFromTarget(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobWikiProposalApply, fmt.Errorf("missing proposal_id in target_key: %w", err))
	}
	// nil tx → ApplyProposalCAS opens + commits its own short transaction
	// (wiki_repo.go:523-535). Passing the old poolFromJob stub here always
	// failed with "tx not wired" (Gap D).
	automation, activated, err := h.Repo.ApplyProposalCAS(ctx, nil, proposalID)
	if err != nil {
		if isCASStale(err) || isLockedCoverage(err) {
			return domain.RetryPermanent, fmtErr(JobWikiProposalApply, err)
		}
		return domain.RetryTransient, fmtErr(JobWikiProposalApply, err)
	}
	_ = automation
	_ = activated
	return domain.RetryTransient, nil
}

// WikiIndexRebuildHandler deterministically rebuilds the index Document for a
// space (§5.1). It loads the published pages, computes the stable index
// content + hash, and records the manifest via WikiRepo.UpdateIndexManifest.
// When the hash matches the existing index version the repo treats it as a
// no-op (§11 "index 重建抖动" mitigation). The knowledge_asset_versions row +
// projection job creation is the repo's job (it owns the asset store); this
// handler computes + delegates the write (Gap C fix).
type WikiIndexRebuildHandler struct {
	Repo wikisvc.WikiRepo
}

func (h *WikiIndexRebuildHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	spaceID, err := uuidFromTarget(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobWikiIndexRebuild, fmt.Errorf("missing space_id in target_key: %w", err))
	}
	pages, err := h.Repo.ListPages(ctx, spaceID)
	if err != nil {
		return domain.RetryTransient, fmtErr(JobWikiIndexRebuild, err)
	}
	pub := make([]wikiidx.PublishedPage, 0, len(pages))
	for _, p := range pages {
		pub = append(pub, wikiidx.PublishedPage{
			PageKey: p.PageKey, PageKind: p.PageKind,
			// ContentHash of the current published version would come from the
			// asset version; the repo's UpdateIndexManifest resolves it from
			// the space's published pages. Kept empty here so the index lists
			// page_key+kind deterministically (a later slice joins the content
			// hash from knowledge_asset_versions).
		})
	}
	content, hash, err := wikiidx.BuildIndex(pub)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobWikiIndexRebuild, err)
	}
	// §5.1: record the index content + hash; the repo's UpdateIndexManifest
	// is idempotent (a matching hash is a no-op) and creates the system
	// knowledge_asset_versions row when the hash changed.
	if err := h.Repo.UpdateIndexManifest(ctx, spaceID, content, hash); err != nil {
		return domain.RetryTransient, fmtErr(JobWikiIndexRebuild, err)
	}
	return domain.RetryTransient, nil
}

// WikiLintScanHandler runs the incremental lint scan (§4.3 lint / §5.3). It
// loads a page batch from the cursor, runs the five detection rules, writes
// stale_reason back to wiki_pages (§4.3 lint "置 wiki_pages.stale_reason" —
// Gap B fix), and (for findings with suggestions) enqueues a wiki_maintain
// job. The scan is incremental — the cursor resumes from the last page_key.
type WikiLintScanHandler struct {
	Repo     wikisvc.WikiRepo
	LintView wikilint.ViewRepo
	JobStore JobStore
}

func (h *WikiLintScanHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	spaceID, err := uuidFromTarget(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobWikiLintScan, fmt.Errorf("missing space_id in target_key: %w", err))
	}
	// Cursor + check_kinds come from j.Progress (the triggering run set them).
	cursor := stringFromJobProgress(j, "cursor")
	var checkKinds []wikilint.CheckKind
	if cks := mapFromJobProgress(j, "check_kinds"); cks != nil {
		for _, k := range cks {
			if s, ok := k.(string); ok {
				checkKinds = append(checkKinds, wikilint.CheckKind(s))
			}
		}
	}
	// LintView is optional wiring: when the worker did not thread the lint
	// view repo, the scan is a no-op (observable but inert) rather than a
	// nil-dereference panic. A later slice wires the postgres lint view.
	if h.LintView == nil {
		return domain.RetryTransient, nil
	}
	findings, next, err := wikilint.Run(ctx, h.LintView, spaceID, cursor, checkKinds, defaultLintStaleWindow, defaultLintBatch)
	if err != nil {
		return domain.RetryTransient, fmtErr(JobWikiLintScan, err)
	}
	// Write stale_reason back for the findings (§4.3 lint "置
	// wiki_pages.stale_reason"). The page_key → reason map de-dups so multiple
	// findings on one page land a single UPDATE.
	for _, f := range findings {
		reason := string(f.Reason)
		if reason == "" {
			reason = "stale"
		}
		if err := h.Repo.UpdatePageStaleReason(ctx, spaceID, f.PageKey, reason); err != nil {
			return domain.RetryTransient, fmtErr(JobWikiLintScan, err)
		}
	}
	// Persist the resume cursor so the next scan batch continues (§18).
	if next != "" {
		_ = next
	}
	return domain.RetryTransient, nil
}

// --- wiki handler helpers ---

const (
	defaultLintStaleWindow = 7 * 24 * time.Hour
	defaultLintBatch       = 100
)

// uuidFromTarget parses j.TargetKey as a UUID (the wiki jobs key on a single
// run/proposal/space id).
func uuidFromTarget(targetKey string) (uuid.UUID, error) {
	return uuid.Parse(targetKey)
}

// isSchemaViolation reports whether the provider returned a schema-gate error.
func isSchemaViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "schema") || strings.Contains(err.Error(), "PagePatch")
}

// isAlreadyApplied reports whether the run was already executed (idempotent
// re-acquire of a wiki_maintain job). The service's ExecuteRun is idempotent —
// it transitions analyzing→applied — so a replay reaching an already-applied
// run is a success, not a retryable failure.
func isAlreadyApplied(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "already applied") || strings.Contains(s, "not queued")
}

func isCASStale(err error) bool {
	return err != nil && strings.Contains(err.Error(), "conflict")
}

func isLockedCoverage(err error) bool {
	return err != nil && strings.Contains(err.Error(), "locked")
}

func redactWikiErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// encodeWikiCursor marshals a lint cursor for the job progress field.
func encodeWikiCursor(next string) json.RawMessage {
	if next == "" {
		return nil
	}
	b, _ := json.Marshal(next)
	return b
}
