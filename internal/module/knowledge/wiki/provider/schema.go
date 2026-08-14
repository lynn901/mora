// Package provider — PagePatch JSON Schema validation gate (design-docs/16
// §4.2). Every PagePatch a provider returns MUST pass this schema; a patch
// that fails is rejected and the whole run is marked 'failed' with no
// candidate landed (§4.2 校验要点 / §11 risk row "Provider 输出绕过 JSON
// Schema").
//
// The validator is hand-written rather than depending on a JSON Schema
// library: the schema is small and fixed (§4.2), the project carries no
// schema-validation dependency (go.mod), and a hand-written validator keeps
// the failure messages precise and side-channel-free (no schema-compiler
// reflection). It is the §4.2 gate; the service layer validates again before
// landing (§11 double validation).
package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// patchActions enumerates the legal action values (§4.2 action enum).
var patchActions = map[string]bool{
	"create": true, "update": true, "link": true, "contradiction": true, "stale": true,
}

// relationKinds enumerates the legal relation_suggestions.kind values (§4.2).
var relationKinds = map[string]bool{
	"derived_from": true, "explains": true, "contradicts": true, "supersedes": true,
}

// lintReasons enumerates the legal lint finding reasons (§4.1 LintFinding).
var lintReasons = map[string]bool{
	"stale": true, "conflict": true, "orphan": true, "missing_source": true, "schema_drift": true,
}

// sha256Hex matches the §4.2 content_hash / contribution_hash pattern.
var sha256Hex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ValidationError is the failure list returned by ValidatePagePatch /
// ValidatePatches. It carries one message per offending field so the service
// can record a precise, non-sensitive error_code (§4.2 "整条 Run 标 failed").
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("provider: invalid PagePatch: %s: %s", e.Field, e.Message)
}

// ValidatePatch runs the §4.2 PagePatch JSON Schema gate against a single
// patch. Returns nil when the patch is conformant; otherwise a *ValidationError
// naming the first offending field. Content body is never inspected — only
// its hash (§4.2 content_hash SHA-256).
func ValidatePatch(p PagePatch) error {
	pk := strings.TrimSpace(p.PageKey)
	if pk == "" {
		return &ValidationError{Field: "page_key", Message: "required, minLength 1"}
	}
	if len(pk) > 256 {
		return &ValidationError{Field: "page_key", Message: "maxLength 256 exceeded"}
	}
	if !patchActions[p.Action] {
		return &ValidationError{Field: "action", Message: "must be one of create|update|link|contradiction|stale"}
	}
	if !sha256Hex.MatchString(p.ContentHash) {
		return &ValidationError{Field: "content_hash", Message: "must be a 64-char lowercase hex SHA-256"}
	}
	// source_versions minItems:1 — every generated page version must anchor at
	// least one source version (invariant 10).
	if len(p.SourceVersions) < 1 {
		return &ValidationError{Field: "source_versions", Message: "minItems 1 — must anchor at least one source version"}
	}
	for i, sv := range p.SourceVersions {
		if sv.SourceAssetID == uuid.Nil {
			return &ValidationError{Field: fmt.Sprintf("source_versions[%d].source_asset_id", i), Message: "required, valid uuid"}
		}
		if sv.SourceAssetVersionID == uuid.Nil {
			return &ValidationError{Field: fmt.Sprintf("source_versions[%d].source_asset_version_id", i), Message: "required, valid uuid"}
		}
		if sv.ContributionHash != "" && !sha256Hex.MatchString(sv.ContributionHash) {
			return &ValidationError{Field: fmt.Sprintf("source_versions[%d].contribution_hash", i), Message: "must be a 64-char lowercase hex SHA-256 when present"}
		}
	}
	for i, rs := range p.RelationSuggestions {
		if !relationKinds[rs.Kind] {
			return &ValidationError{Field: fmt.Sprintf("relation_suggestions[%d].kind", i), Message: "must be one of derived_from|explains|contradicts|supersedes"}
		}
		if rs.ToAssetID == uuid.Nil {
			return &ValidationError{Field: fmt.Sprintf("relation_suggestions[%d].to_asset_id", i), Message: "required, valid uuid"}
		}
		if rs.ToVersionID != nil && *rs.ToVersionID == uuid.Nil {
			return &ValidationError{Field: fmt.Sprintf("relation_suggestions[%d].to_version_id", i), Message: "must be a valid uuid when present"}
		}
	}
	return nil
}

// ValidatePatches runs ValidatePatch over a slice and returns the first error
// (a run fails on the first non-conformant patch — §4.2 "未通过的 patch 整条
// Run 标 failed").
func ValidatePatches(patches []PagePatch) error {
	for _, p := range patches {
		if err := ValidatePatch(p); err != nil {
			return err
		}
	}
	return nil
}

// ValidateLintFinding runs the §4.1 LintFinding shape gate: reason must be one
// of the five check kinds, and an optional suggestion must itself be a valid
// PagePatch.
func ValidateLintFinding(f LintFinding) error {
	if !lintReasons[f.Reason] {
		return &ValidationError{Field: "reason", Message: "must be one of stale|conflict|orphan|missing_source|schema_drift"}
	}
	if f.Suggestion != nil {
		if err := ValidatePatch(*f.Suggestion); err != nil {
			return &ValidationError{Field: "suggestion", Message: err.Error()}
		}
	}
	return nil
}

// ValidateLintReport runs ValidateLintFinding over the report's findings.
func ValidateLintReport(r WikiLintReport) error {
	for _, f := range r.Findings {
		if err := ValidateLintFinding(f); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePatchJSON is the raw-JSON entry point for callers that decode the
// provider response generically (the service lands a row from the struct, but
// the schema gate is defined over JSON). It unmarshals a []byte or json.RawMessage
// into []PagePatch and runs ValidatePatches. A malformed payload is a schema
// failure (the whole run is marked failed).
func ValidatePatchJSON(raw json.RawMessage) ([]PagePatch, error) {
	var patches []PagePatch
	if err := json.Unmarshal(raw, &patches); err != nil {
		return nil, &ValidationError{Field: "$", Message: fmt.Sprintf("invalid JSON array: %v", err)}
	}
	if err := ValidatePatches(patches); err != nil {
		return nil, err
	}
	return patches, nil
}
