package asset

// activation.go extends the Phase 1 asset write surface with the async
// version-activation path (design-docs/14 §7 CAS + §3.3 reconcile). The dual-
// write port in registry.go registers a NATIVE document version and CAS-activates
// its current_version_id immediately (native docs are ready+published at write
// time). The Connector-sourced path is different: a version starts as
// build_status='pending', governance_status='candidate'; projections build
// asynchronously; only when ALL required projections are ready AND governance is
// published does the CAS activation flip current_version_id (§7 red-line:
// failed/partial/unpublished must NOT overwrite the last usable version).
//
// These methods back the knowledge-worker handlers (§5.2):
//   - MarkProjectionReady ← ProjectionBuildHandler (rag-worker / Provider bridge)
//   - Activate            ← AssetActivateHandler (the CAS itself)
//   - ReconcileScan       ← ReconcileHandler (§3.3 consistency sweep)
//
// Existence never leaks (§8.2): a missing version/projection surfaces as a
// sentinel the handlers map to a permanent job failure, never as a 403-style
// leak. The CAS is the final authority — a stale expected_current fails the CAS
// and leaves the asset untouched (no rewind).

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
)

// Sentinel errors for the §7 CAS activation path. The worker handlers map
// these to retry dispositions: the CAS-already-decided + governance errors are
// permanent (the asset state won't change by retrying); everything else is
// transient (the worker retries).
var (
	// ErrVersionNotFound: the asset_version row does not exist (or the asset
	// does not resolve for this caller). Mapped to permanent — existence must
	// not leak (§8.2), and a missing version will not appear by retrying.
	ErrVersionNotFound = errors.New("asset: version not found")
	// ErrCASVersionStale: the version's version_no is behind the asset's
	// latest_requested_version_no barrier — a newer version already activated,
	// so this late-completing old version must NOT switch current_version_id
	// (§7 单调栅栏). Permanent: the CAS already decided.
	ErrCASVersionStale = errors.New("asset: cas version stale — newer version already activated")
	// ErrCASExpectedMismatch: the caller's expected_current does not match the
	// asset's actual current_version_id. This is the §7 CAS guard for explicit
	// rollback (review_decisions promote/deprecate): a rollback MUST name the
	// version it expects to replace, or it is rejected (§10.3 用例 24).
	// Permanent.
	ErrCASExpectedMismatch = errors.New("asset: cas expected_current mismatch")
	// ErrNotPublished: the version's governance_status is not 'published'. The
	// CAS only activates a published version (§7: governance gate). Permanent:
	// governance is an explicit human/system action, not a retry target.
	ErrNotPublished = errors.New("asset: version not published")
	// ErrProjectionsNotReady: not all required projections are ready yet. The
	// Activate CAS refuses to flip current_version_id until every required
	// projection is ready (§10.3 用例 20). Transient at the build-time call site
	// (MarkProjectionReady may flip build_status to ready only when the last
	// projection lands); permanent if Activate is called prematurely.
	ErrProjectionsNotReady = errors.New("asset: required projections not ready")
)

// Activation is the input to Activate — the §7 CAS activation request.
//
// Fence is the latest_requested_version_no the caller captured when it
// requested the version; the CAS only switches current_version_id when
// latest_requested_version_no = Fence (§7 单调栅栏). A late-completing old
// version whose Fence is behind the current barrier fails with
// ErrCASVersionStale and is marked ready-only (no switch).
//
// ExpectedCurrent is the current_version_id the caller expects to replace
// (uuid.Nil = "no current version" / initial activation). An explicit rollback
// MUST name the version it is replacing (§10.3 用例 24); a mismatch fails with
// ErrCASExpectedMismatch. For a forward activation (new version), ExpectedCurrent
// is the previous current_version_id or uuid.Nil.
type Activation struct {
	AssetID        domain.UUID
	VersionID      domain.UUID // the knowledge_asset_versions.id to activate
	Fence          int64        // latest_requested_version_no at request time
	ExpectedCurrent domain.UUID // uuid.Nil = no current / initial
}

// ActivationResult reports the CAS outcome so the handler can decide the retry
// disposition and record an audit trail.
type ActivationResult struct {
	// Activated is true when current_version_id was flipped to VersionID. false
	// means the CAS did not switch (stale or expected-mismatch) — the version
	// was still marked build_status='ready' if its projections+governance
	// allowed, but the pointer was NOT moved (§7 fail-no-overwrite).
	Activated bool
	// PreviousCurrentID is the current_version_id before the CAS attempt
	// (uuid.Nil if there was none). Lets the handler report "replaced X" or
	// "initial activation".
	PreviousCurrentID domain.UUID
}

// ProjectionReady is the input to MarkProjectionReady — the write-back when one
// projection build finishes (rag-worker / Provider → asset_projections).
type ProjectionReady struct {
	AssetVersionID domain.UUID
	Kind           domain.ProjectionKind
	BuildRevision  string
	Provider       string
	ProviderVersion string
	// Locator is the non-executable placement info (Qdrant collection / point
	// filter, FTS table, MinIO key prefix) — never content (§2.1).
	Locator map[string]any
}

// MarkProjectionReadyResult reports whether marking one projection ready also
// flipped the version's build_status to 'ready' (i.e. this was the last
// required projection). The handler uses ActivatedBuildReady to decide whether
// to enqueue the asset_activate job immediately.
type MarkProjectionReadyResult struct {
	// BuildReady is true when, after this projection landed, ALL required
	// projections for the version are ready — so the version's build_status
	// was flipped (or was already) 'ready'. false = still pending other
	// projections; build_status stays pending/building.
	BuildReady bool
}

// ReconcileOutcome is the §3.3 consistency-sweep result for one workspace.
type ReconcileOutcome struct {
	// FixedAssets is the number of assets whose current_version_id was CAS'd to
	// the document's current version (§3.3 row: asset.current_version_id ↔
	// documents.version_no drift).
	FixedAssets int
	// RequeuedProjections is the count of projection_build jobs re-enqueued for
	// versions whose build_status='ready' but a required projection is
	// missing/failed (§3.3 row: ready-but-missing-projection → requeue).
	RequeuedProjections int
	// StaleProjectionsMarked is the count of asset_projections rows marked
	// 'stale' for superseded versions (§3.3 cleanup).
	StaleProjectionsMarked int
}

// ActivationRegistry is the async activation + reconcile port (§7 + §3.3),
// layered on top of the dual-write Registry. It takes a pgx.Tx for the
// transactional CAS so the activation commits atomically with any caller-side
// bookkeeping (job fence, outbox event). ReconcileScan runs its own short
// transaction(s) and takes only the context (it is a sweep, not a single-row
// write) — callers pass a pool-backed tx starter.
type ActivationRegistry interface {
	// MarkProjectionReady upserts an asset_projections row as 'ready' for
	// (asset_version_id, kind, build_revision) and, if this was the last
	// required projection, flips the version's build_status to 'ready'
	// (§10.3 用例 20: a missing required projection blocks ready). Idempotent:
	// re-marking the same projection ready is a no-op. A missing version
	// returns ErrVersionNotFound (no existence leak).
	MarkProjectionReady(ctx context.Context, tx pgx.Tx, r ProjectionReady) (MarkProjectionReadyResult, error)

	// Activate performs the §7 CAS activation of current_version_id:
	//   - the version must exist and be governance_status='published' (else
	//     ErrNotPublished);
	//   - all required projections must be ready (else ErrProjectionsNotReady);
	//   - the CAS WHERE latest_requested_version_no = Fence AND
	//     current_version_id IS NOT DISTINCT FROM ExpectedCurrent either
	//     flips the pointer (Activated=true) or, if the barrier advanced past
	//     Fence or ExpectedCurrent mismatches, leaves the pointer untouched
	//     (Activated=false, ErrCASVersionStale / ErrCASExpectedMismatch) —
	//     §10.3 用例 22/24.
	// The version is marked build_status='ready' as part of a successful
	// activation; a failed CAS does NOT change build_status (the version may
	// still be ready from MarkProjectionReady, but it is not the current one).
	Activate(ctx context.Context, tx pgx.Tx, a Activation) (ActivationResult, error)

	// ReconcileScan runs the §3.3 consistency sweep for one workspace: CAS-fix
	// assets whose current_version_id drifted from the document's current
	// version, requeue missing projections for ready versions, and mark stale
	// projections of superseded versions. Returns the aggregate outcome. The
	// sweep runs in its own transactions; the caller's tx (if any) is not used.
	ReconcileScan(ctx context.Context, pool ReconcilePool, workspaceID domain.UUID) (ReconcileOutcome, error)
}

// ReconcilePool is the minimal tx-starter surface ReconcileScan needs (a
// *pgxpool.Pool satisfies it). Kept as an interface so the handler can pass the
// pool and tests can substitute a fake.
type ReconcilePool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}
