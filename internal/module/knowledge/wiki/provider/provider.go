// Package provider defines the Wiki maintenance provider port
// (design-docs/16 §4.1 / 12 §10.4). The provider only receives pages, source
// versions and a non-executable Schema that Mora has already authorized and
// budget-trimmed; it never touches the database, object storage, URLs or Git.
// Its return values are constrained by a JSON Schema (§4.2) and express only
// candidate patches, references and diagnostics. Path normalization,
// expected-version CAS, relation persistence, review and publication are all
// Mora's responsibility — the provider cannot widen its read scope
// (prompt-injection guard, decision D2 / §8.1).
package provider

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// Capability is the capability envelope the provider operates under (§10.4).
// Mora computes WorkspaceID + AuthzRevision from the acting principal's RBAC
// and trims the input set to fit MaxReadBytes / MaxReadPages. The provider
// MUST NOT read beyond this scope; source_versions are fixed at call time.
type Capability struct {
	WorkspaceID  uuid.UUID
	AuthzRevision int64
	MaxReadBytes int
	MaxReadPages int
}

// PageRef names an affected page plus a compact summary the provider may use
// to decide whether to regenerate it. The summary never carries full body text
// — for locked pages only the page_key + current version summary is passed
// (§4.4 locked protection point 1), so the provider cannot rewrite the body.
type PageRef struct {
	PageKey        string          `json:"page_key"`
	PageKind       string          `json:"page_kind"`
	AutomationState string         `json:"automation_state"`
	CurrentVersionID *uuid.UUID    `json:"current_version_id,omitempty"`
	Summary        json.RawMessage  `json:"summary,omitempty"`
}

// SourceVersionRef is an already-authorized, budget-trimmed source version the
// provider may read. ContributionHash is set by Mora per (page, source) pair.
type SourceVersionRef struct {
	SourceAssetID        uuid.UUID `json:"source_asset_id"`
	SourceAssetVersionID uuid.UUID `json:"source_asset_version_id"`
	ContributionHash     string    `json:"contribution_hash,omitempty"`
}

// RelationSuggestion is a proposed knowledge_relations entry (§4.1). Mora
// persists it when the candidate version is activated (§10.4 relation landing
// is Mora's job).
type RelationSuggestion struct {
	Kind        string     `json:"kind"` // derived_from|explains|contradicts|supersedes
	ToAssetID   uuid.UUID  `json:"to_asset_id"`
	ToVersionID *uuid.UUID `json:"to_version_id,omitempty"`
}

// PagePatch is a candidate revision constrained by the PagePatch JSON Schema
// (§4.2). Each patch carries the target page_key, the expected current
// version (CAS precondition), the full source version list it was derived
// from (traceability, invariant 10), the action, the candidate content hash
// and relation suggestions. Content body is NOT in the patch — only its hash;
// Mora creates the candidate knowledge_asset_versions row from its own store.
type PagePatch struct {
	PageKey              string              `json:"page_key"`
	ExpectedVersionID    *uuid.UUID          `json:"expected_version_id,omitempty"`
	Action               string              `json:"action"` // create|update|link|contradiction|stale
	ContentHash          string              `json:"content_hash"`
	SourceVersions       []SourceVersionRef  `json:"source_versions"`
	RelationSuggestions  []RelationSuggestion `json:"relation_suggestions"`
}

// WikiIngestRequest is the input to ProposeIngest: the affected pages plus the
// authorized source versions whose arrival triggered the run (§4.3 ingest).
type WikiIngestRequest struct {
	WikiSpaceID    uuid.UUID         `json:"wiki_space_id"`
	Schema         json.RawMessage    `json:"schema"`
	AffectedPages []PageRef          `json:"affected_pages"`
	SourceVersions []SourceVersionRef `json:"source_versions"`
}

// WikiAnswerRequest is the input to ProposeAnswer: an explicit "settle answer"
// request for one page (§4.3 query_file). AnswerRef is a non-executable
// reference {asset_id, version_id, excerpt_hash}.
type WikiAnswerRequest struct {
	WikiSpaceID    uuid.UUID         `json:"wiki_space_id"`
	Schema         json.RawMessage    `json:"schema"`
	PageKey        string            `json:"page_key"`
	AnswerRef      json.RawMessage    `json:"answer_ref"`
	SourceVersions []SourceVersionRef `json:"source_versions"`
}

// WikiLintRequest is the input to Lint: the schema, an incremental cursor and
// the check kinds to run (§4.3 lint / §5.3).
type WikiLintRequest struct {
	WikiSpaceID uuid.UUID       `json:"wiki_space_id"`
	Schema      json.RawMessage  `json:"schema"`
	PagesCursor json.RawMessage  `json:"pages_cursor,omitempty"`
	CheckKinds  []string         `json:"check_kinds"`
}

// LintFinding is one detection from a lint run (§4.1).
type LintFinding struct {
	PageKey    string          `json:"page_key"`
	Reason     string          `json:"reason"` // stale|conflict|orphan|missing_source|schema_drift
	Detail     json.RawMessage  `json:"detail"`
	Suggestion *PagePatch       `json:"suggestion,omitempty"`
}

// WikiLintReport is the aggregate lint result.
type WikiLintReport struct {
	Findings []LintFinding `json:"findings"`
}

// WikiMaintenanceProvider is the port (§10.4). Implementations may wrap an
// external model or be a local deterministic implementation. The provider
// MUST NOT depend on wiki/service — the dependency is one-way.
type WikiMaintenanceProvider interface {
	ProposeIngest(ctx context.Context, cap Capability, req WikiIngestRequest) ([]PagePatch, error)
	ProposeAnswer(ctx context.Context, cap Capability, req WikiAnswerRequest) ([]PagePatch, error)
	Lint(ctx context.Context, cap Capability, req WikiLintRequest) (WikiLintReport, error)
	Health(ctx context.Context) error
}
