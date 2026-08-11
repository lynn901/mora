package domain

// governance.go defines the Phase 1 Review governance value objects
// (design-docs/14 §2.1 review_requests/review_decisions, §8 internal/domain/
// governance.go). Review decisions are immutable append-only rows; an asset's
// current governance_status is only the projection of the latest decision.

import "time"

// ReviewRequestStatus is the state of a review request (14 §2.1).
type ReviewRequestStatus string

const (
	ReviewPending    ReviewRequestStatus = "pending"
	ReviewApproved   ReviewRequestStatus = "approved"
	ReviewRejected   ReviewRequestStatus = "rejected"
	ReviewSuperseded ReviewRequestStatus = "superseded"
)

// ReviewDecision is the immutable action recorded against a request (14 §2.1).
type ReviewDecision string

const (
	DecisionApprove  ReviewDecision = "approve"
	DecisionReject   ReviewDecision = "reject"
	DecisionMerge    ReviewDecision = "merge"
	DecisionPromote  ReviewDecision = "promote"
	DecisionDeprecate ReviewDecision = "deprecate"
)

// ReviewRequest is a request to approve a specific asset version under a
// governance profile (14 §2.1). legacy_migration backfill creates one per
// document version with status=approved and a system service-account decision.
type ReviewRequest struct {
	ID                  UUID
	WorkspaceID         UUID
	AssetID             UUID
	AssetVersionID      UUID
	GovernanceProfileID UUID
	RequestedByType     SubjectType
	RequestedByID       UUID
	Status              ReviewRequestStatus
	Rationale           string
	CreatedAt           time.Time
	ResolvedAt          *time.Time
	ResolvedByType      SubjectType
	ResolvedByID        *UUID
}

// ReviewDecisionRecord is an immutable decision row (14 §2.1). One request may
// accumulate multiple decisions over its life; the latest determines the
// request's projected status.
type ReviewDecisionRecord struct {
	ID                UUID
	ReviewRequestID   UUID
	Decision          ReviewDecision
	DecisionByType    SubjectType
	DecisionByID      UUID
	PolicyVersion     string
	RationaleRedacted string
	CreatedAt         time.Time
}

// PolicyVersionLegacyMigration is the policy snapshot tag the legacy_migration
// system Profile stamps on its auto-approve decisions (14 §3.4).
const PolicyVersionLegacyMigration = "legacy_migration-v1"

// LegacyMigrationRationale is the redacted rationale stamped on backfill
// review_requests (14 §3.4).
const LegacyMigrationRationale = "legacy_migration backfill"
