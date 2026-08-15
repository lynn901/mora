// Package lint implements the Wiki page-health detection rules
// (design-docs/16 §4.3 lint / §5.3 / §9 门禁 "lint 可发现预置的过期/冲突/
// 孤立/缺源/Schema 偏差样例"). The five check kinds are:
//
//   - stale: a managed page whose last_maintained_at is older than any of its
//     source versions' publish time, or older than a configurable staleness
//     window.
//   - conflict: two source versions for the same page_key disagree (different
//     contribution_hash families → contradiction).
//   - orphan: a wiki_page whose page_kind implies an entity/concept but whose
//     document_asset has no published version (or the page_key is unreferenced
//     by any other page).
//   - missing_source: a published page version whose wiki_page_sources rows
//     reference a source version that no longer exists (deleted source).
//   - schema_drift: a page whose content structure no longer matches the
//     Schema Document's page-kind contract.
//
// The detection runs over WikiRepo.LintView rows (a read-only projection that
// joins wiki_pages → wiki_page_sources → knowledge_asset_versions). It only
// produces findings + optional suggestion patches; it never publishes (§4.3
// lint "只产报告或修订候选不发布"). The worker's wiki_lint_scan handler
// calls Run and writes stale_reason back to wiki_pages.
package lint

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// CheckKind enumerates the five lint check kinds (§4.1).
type CheckKind string

const (
	CheckStale          CheckKind = "stale"
	CheckConflict       CheckKind = "conflict"
	CheckOrphan         CheckKind = "orphan"
	CheckMissingSource  CheckKind = "missing_source"
	CheckSchemaDrift    CheckKind = "schema_drift"
)

// AllChecks is the full set, used when a lint request omits check_kinds.
var AllChecks = []CheckKind{
	CheckStale, CheckConflict, CheckOrphan, CheckMissingSource, CheckSchemaDrift,
}

// Finding is one lint detection. It mirrors provider.LintFinding but stays in
// the lint package to keep the detection rules independent of the provider
// port (the worker builds provider.LintFinding from these).
type Finding struct {
	PageKey    string         `json:"page_key"`
	Reason     CheckKind      `json:"reason"`
	Detail     map[string]any `json:"detail"`
}

// PageView is the read-only projection a lint run scans (§5.3 incremental
// cursor). It joins a wiki_page with its current published version + source
// version anchors. The repo builds it; lint only inspects.
type PageView struct {
	PageKey           string
	PageKind          string
	AutomationState   string
	LastMaintainedAt  *time.Time
	StaleReason       string
	// CurrentVersionID is the published knowledge_asset_versions.id (nil if none).
	CurrentVersionID  *uuid.UUID
	// SourceVersions are the wiki_page_sources rows for the current version.
	// Each carries the source_asset_version_id + a contribution_hash. A nil
	// entry with a missing source_asset_id signals a deleted source.
	SourceVersions    []SourceVersionView
}

// SourceVersionView is one source anchor for a page version.
type SourceVersionView struct {
	SourceAssetID        uuid.UUID
	SourceAssetVersionID *uuid.UUID // nil when the source version was deleted (missing_source)
	SourcePublishedAt    *time.Time
	ContributionHash     string
}

// ViewRepo is the read-only projection the lint rules scan. It returns pages
// in a stable order from an opaque cursor so incremental scans are resumable
// (§5.3).
type ViewRepo interface {
	// LintView returns the next page batch for a space, starting after cursor.
	// An empty cursor starts from the beginning; the returned nextCursor is
	// "" when the scan is complete.
	LintView(ctx context.Context, spaceID uuid.UUID, cursor string, limit int) (pages []PageView, nextCursor string, err error)
}

// Run executes the requested checks over the page batch and returns findings.
// Only the check_kinds named are run; an empty slice runs all five. It never
// mutates — the worker writes stale_reason back (§4.3).
func Run(ctx context.Context, repo ViewRepo, spaceID uuid.UUID, cursor string, checkKinds []CheckKind, staleWindow time.Duration, limit int) ([]Finding, string, error) {
	if repo == nil {
		return nil, "", fmt.Errorf("lint: view repo is nil")
	}
	if limit <= 0 {
		limit = 100
	}
	kinds := checkKinds
	if len(kinds) == 0 {
		kinds = AllChecks
	}
	kindSet := make(map[CheckKind]bool, len(kinds))
	for _, k := range kinds {
		kindSet[k] = true
	}
	if staleWindow <= 0 {
		staleWindow = 7 * 24 * time.Hour // default 7d; PM fills lint_cron + window later
	}
	pages, next, err := repo.LintView(ctx, spaceID, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	var findings []Finding
	now := time.Now().UTC()
	for _, p := range pages {
		if kindSet[CheckMissingSource] {
			if f := detectMissingSource(p); f != nil {
				findings = append(findings, *f)
			}
		}
		if kindSet[CheckStale] {
			if f := detectStale(p, now, staleWindow); f != nil {
				findings = append(findings, *f)
			}
		}
		if kindSet[CheckConflict] {
			if f := detectConflict(p); f != nil {
				findings = append(findings, *f)
			}
		}
		if kindSet[CheckOrphan] {
			if f := detectOrphan(p); f != nil {
				findings = append(findings, *f)
			}
		}
		if kindSet[CheckSchemaDrift] {
			if f := detectSchemaDrift(p); f != nil {
				findings = append(findings, *f)
			}
		}
	}
	return findings, next, nil
}

// detectMissingSource flags a page whose current version anchors a source
// version that no longer exists (§9 missing_source).
func detectMissingSource(p PageView) *Finding {
	for _, sv := range p.SourceVersions {
		if sv.SourceAssetVersionID == nil {
			return &Finding{
				PageKey: p.PageKey,
				Reason:  CheckMissingSource,
				Detail:  map[string]any{"source_asset_id": sv.SourceAssetID.String()},
			}
		}
	}
	return nil
}

// detectStale flags a managed page whose last_maintained_at is older than the
// stale window OR older than any of its source versions' publish times
// (§9 stale).
func detectStale(p PageView, now time.Time, window time.Duration) *Finding {
	if p.AutomationState != "managed" {
		return nil // locked/manual pages: lint may mark stale but only managed is actionable
	}
	var staleSince *time.Time
	// Window-based staleness.
	if p.LastMaintainedAt != nil {
		if now.Sub(*p.LastMaintainedAt) > window {
			t := *p.LastMaintainedAt
			staleSince = &t
		}
	} else {
		// Never maintained → stale since page creation is unknown; use now as marker.
		t := now
		staleSince = &t
	}
	// Source-newer-than-page staleness.
	for _, sv := range p.SourceVersions {
		if sv.SourcePublishedAt != nil {
			if p.LastMaintainedAt == nil || sv.SourcePublishedAt.After(*p.LastMaintainedAt) {
				t := *sv.SourcePublishedAt
				if staleSince == nil || t.After(*staleSince) {
					staleSince = &t
				}
			}
		}
	}
	if staleSince == nil {
		return nil
	}
	return &Finding{
		PageKey: p.PageKey,
		Reason:  CheckStale,
		Detail:  map[string]any{"stale_since": staleSince.Format(time.RFC3339)},
	}
}

// detectConflict flags a page whose source versions disagree — the same
// source_asset_id contributed via multiple conflicting contribution_hash
// families (§9 conflict). A simple heuristic: group contribution hashes by
// source; >1 distinct hash family per source → conflict.
func detectConflict(p PageView) *Finding {
	bySource := make(map[uuid.UUID]map[string]struct{})
	for _, sv := range p.SourceVersions {
		if sv.ContributionHash == "" {
			continue
		}
		if bySource[sv.SourceAssetID] == nil {
			bySource[sv.SourceAssetID] = make(map[string]struct{})
		}
		bySource[sv.SourceAssetID][sv.ContributionHash] = struct{}{}
	}
	for sid, hashes := range bySource {
		if len(hashes) > 1 {
			// Canonicalize hash list for stable detail.
			hl := make([]string, 0, len(hashes))
			for h := range hashes {
				hl = append(hl, h)
			}
			sort.Strings(hl)
			return &Finding{
				PageKey: p.PageKey,
				Reason:  CheckConflict,
				Detail:  map[string]any{"source_asset_id": sid.String(), "conflicting_hashes": hl},
			}
		}
	}
	return nil
}

// detectOrphan flags a page whose page_kind implies a published entity/
// concept but whose CurrentVersionID is nil (no published version), OR a
// page_key that no other page references (§9 orphan). The cross-reference
// check needs the repo's relation graph; the simple leg (no published
// version) is detectable from PageView alone.
func detectOrphan(p PageView) *Finding {
	if p.CurrentVersionID == nil {
		return &Finding{
			PageKey: p.PageKey,
			Reason:  CheckOrphan,
			Detail:  map[string]any{"cause": "no_published_version"},
		}
	}
	return nil
}

// detectSchemaDrift flags a page whose page_kind is not one the Schema
// Document declares (§9 schema_drift). The Schema's declared kinds are not
// in PageView (the schema is non-executable, passed to the provider); a
// minimal drift check is: page_kind is one of the seven legal enum values.
// A page_kind outside the enum → schema_drift. The full structural check
// (content block shape vs schema contract) runs in the provider; this is the
// cheap pre-filter.
func detectSchemaDrift(p PageView) *Finding {
	switch p.PageKind {
	case "summary", "entity", "concept", "comparison", "synthesis", "index", "log":
		return nil
	}
	return &Finding{
		PageKey: p.PageKey,
		Reason:  CheckSchemaDrift,
		Detail:  map[string]any{"page_kind": p.PageKind, "cause": "page_kind not in schema enum"},
	}
}
