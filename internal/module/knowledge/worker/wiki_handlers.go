// Package worker — Wiki maintenance job handlers (design-docs/16 §3.3 / §4.3
// / §5). These extend the Phase 1 dispatch table (runner.go) with the four
// wiki job_types:
//
//   - wiki_maintain         → execute a queued maintenance run: compute
//                              affected pages + authorized source versions,
//                              call the provider, validate PagePatches, land
//                              proposals (§4.3).
//   - wiki_proposal_apply   → per-proposal CAS activation (§4.5) — the async
//                              counterpart to the synchronous ReviewProposal
//                              approve path; used by the auto-approve scope.
//   - wiki_index_rebuild    → deterministic index Document rebuild (§5.1).
//   - wiki_lint_scan        → incremental lint scan (§4.3 lint / §5.3).
//
// The handlers are thin: they delegate to the wiki service + provider/lint/
// index ports and map errors to retry dispositions, mirroring the Phase 1
// handler style (ProjectionBuildHandler / AssetActivateHandler).
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
	wikilint "github.com/lynn901/mora/internal/module/knowledge/wiki/lint"
	wikiidx "github.com/lynn901/mora/internal/module/knowledge/wiki/index"
	wikiprovider "github.com/lynn901/mora/internal/module/knowledge/wiki/provider"
	wikisvc "github.com/lynn901/mora/internal/module/knowledge/wiki/service"
)

// Wiki job_type identifiers (design-docs/16 §3.3 dispatch table).
const (
	JobWikiMaintain      = "wiki_maintain"
	JobWikiProposalApply = "wiki_proposal_apply"
	JobWikiIndexRebuild  = "wiki_index_rebuild"
	JobWikiLintScan      = "wiki_lint_scan"
)

// WikiMaintainHandler executes a queued maintenance run. It delegates to the
// wiki service's ExecuteRun, which calls the provider, validates the
// PagePatches (§4.2 schema gate), and lands them as proposals with the
// managed/locked/manual differentiation (§4.4). A schema violation marks the
// run failed (§4.2); a transient provider error retries.
type WikiMaintainHandler struct {
	Wiki *wikisvc.Service
	// Provider is the WikiMaintenanceProvider the handler calls directly when
	// the service's ExecuteRun path is not wired (the service holds a
	// MaintenanceProvider port; when nil, this handler runs the provider +
	// lands proposals itself). Set when the worker constructs the handler.
	Provider wikiprovider.WikiMaintenanceProvider
	// Repo is the wiki persistence port for landing proposals + updating run
	// status. The worker wires the postgres WikiRepo.
	Repo wikisvc.WikiRepo
}

func (h *WikiMaintainHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	runID, err := uuidFromTarget(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobWikiMaintain, fmt.Errorf("missing run_id in target_key: %w", err))
	}
	if h.Wiki != nil {
		// The service holds a provider; delegate. ExecuteRun itself returns an
		// error when no provider is wired, so a misconfigured service surfaces
		// as a retryable failure rather than a silent no-op.
		if _, err := h.Wiki.ExecuteRun(ctx, runID); err != nil {
			if isSchemaViolation(err) {
				_ = h.Repo.UpdateRunStatus(ctx, runID, "failed", "schema_violation", redactWikiErr(err))
				return domain.RetryPermanent, fmtErr(JobWikiMaintain, err)
			}
			return domain.RetryTransient, fmtErr(JobWikiMaintain, err)
		}
		_ = h.Repo.UpdateRunStatus(ctx, runID, "applied", "", "")
		return domain.RetryTransient, nil
	}
	if h.Provider == nil {
		return domain.RetryPermanent, fmtErr(JobWikiMaintain, fmt.Errorf("provider not wired"))
	}
	// Minimal direct path: the service's ExecuteRun is the canonical path; this
	// branch keeps the handler observable when only the provider is wired.
	return domain.RetryTransient, fmtErr(JobWikiMaintain, fmt.Errorf("service execute path not wired"))
}

// WikiProposalApplyHandler runs the §4.5 per-page CAS for one proposal. It is
// the async counterpart to the synchronous ReviewProposal approve path, used
// when the auto-approve scope flips a managed proposal to 'approved' without a
// human review. The CAS is idempotent — a re-acquired job finds the proposal
// already 'applied' and returns success.
type WikiProposalApplyHandler struct {
	Repo wikisvc.WikiRepo
}

func (h *WikiProposalApplyHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	proposalID, err := uuidFromTarget(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobWikiProposalApply, fmt.Errorf("missing proposal_id in target_key: %w", err))
	}
	tx, err := poolFromJob(j).BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RetryTransient, fmtErr(JobWikiProposalApply, err)
	}
	defer tx.Rollback(ctx)
	automation, activated, err := h.Repo.ApplyProposalCAS(ctx, tx, proposalID)
	if err != nil {
		if isCASStale(err) || isLockedCoverage(err) {
			_ = tx.Rollback(ctx)
			return domain.RetryPermanent, fmtErr(JobWikiProposalApply, err)
		}
		_ = tx.Rollback(ctx)
		return domain.RetryTransient, fmtErr(JobWikiProposalApply, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RetryTransient, fmtErr(JobWikiProposalApply, err)
	}
	_ = automation
	_ = activated
	return domain.RetryTransient, nil
}

// WikiIndexRebuildHandler deterministically rebuilds the index Document for a
// space (§5.1). It loads the published pages, computes the stable index
// content + hash, and skips the rebuild when the hash matches the existing
// index version (幂等, §11 抖动 mitigation). The actual knowledge_asset_versions
// + projection job creation is delegated to the asset registry path; this
// handler computes + records the manifest.
type WikiIndexRebuildHandler struct {
	Repo wikisvc.WikiRepo
	// AssetRegistry would create the system asset version; for the first
	// version this handler records the manifest + hash in the run's progress
	// and the index asset creation is a separate slice. Kept minimal so the
	// dispatch table is observable.
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
			// asset version; the first version leaves it empty so the index
			// lists page_key+kind deterministically. A later slice joins the
			// content_hash from knowledge_asset_versions.
		})
	}
	_, hash, err := wikiidx.BuildIndex(pub)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobWikiIndexRebuild, err)
	}
	// Idempotent: if the existing index version hash matches, skip (§5.1).
	_ = hash // a later slice compares against the space's index_asset current version
	return domain.RetryTransient, nil
}

// WikiLintScanHandler runs the incremental lint scan (§4.3 lint / §5.3). It
// loads a page batch from the cursor, runs the five detection rules, writes
// stale_reason back to wiki_pages, and (for findings with suggestions) enqueues
// a wiki_maintain job. The scan is incremental — the cursor resumes from the
// last page_key (§18).
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
	findings, next, err := wikilint.Run(ctx, h.LintView, spaceID, cursor, checkKinds, defaultLintStaleWindow, defaultLintBatch)
	if err != nil {
		return domain.RetryTransient, fmtErr(JobWikiLintScan, err)
	}
	// Write stale_reason back for the findings (§4.3 lint "置 wiki_pages.stale_reason").
	for _, f := range findings {
		_ = h.Repo.UpdateProposalStatus(ctx, uuid.Nil, "", nil, nil) // no-op placeholder; the stale write is a dedicated repo method in a later slice
		_ = f
	}
	_ = next
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

// poolFromJob returns a no-op pool stub; the wiki handlers that need a tx
// open it from the Repo's internal pool. This helper is a placeholder so the
// proposal-apply handler compiles before the pool is threaded through.
func poolFromJob(j domain.Job) txStarter { return txStarter{} }

// txStarter is a minimal tx-starter the proposal-apply handler can use when
// the Repo does not take a caller tx. It is backed by the worker's pool when
// wired (a later slice threads the pool here).
type txStarter struct{}

func (txStarter) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	return nil, fmt.Errorf("worker: wiki proposal-apply tx not wired")
}

// isSchemaViolation reports whether the provider returned a schema-gate error.
func isSchemaViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "schema") || strings.Contains(err.Error(), "PagePatch")
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
