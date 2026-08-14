// Package worker — provider adapter. The wiki service declares a local
// MaintenanceProvider port (ProposeIngest / ProposeAnswer over service-typed
// AffectedPage / PagePatch) so it does not import the concrete provider
// package (one-way dependency, provider.go §10.4). The worker, which already
// imports both, bridges the real provider.WikiMaintenanceProvider to that
// port here. This is the seam the test-engineer's Gap A flagged: without it
// the service's ExecuteRun had no callable provider path and
// NoopProvider (a WikiMaintenanceProvider) could not satisfy the service port.
package worker

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	wikiprovider "github.com/lynn901/mora/internal/module/knowledge/wiki/provider"
	wikisvc "github.com/lynn901/mora/internal/module/knowledge/wiki/service"
)

// ProviderAdapter wraps a provider.WikiMaintenanceProvider and exposes it as a
// service.MaintenanceProvider. It owns no state of its own; the Capability +
// Schema are resolved per call from the affected-page set + the run's pinned
// schema version. A nil inner provider yields empty patch sets (the run
// finishes applied with zero proposals — the same contract NoopProvider
// honours, §4.1).
type ProviderAdapter struct {
	Inner wikiprovider.WikiMaintenanceProvider
	// Schema is the Space's non-executable PagePatch Schema (§4.2), resolved
	// by the worker from the space's schema_asset_id. May be nil for the
	// NoopProvider path (no schema validation happens inside the provider; the
	// service runs the §4.2 gate after the call).
	Schema json.RawMessage
	// SchemaVersionID + WorkspaceID form the Capability envelope (§10.4).
	SchemaVersionID uuid.UUID
	WorkspaceID     uuid.UUID
}

// Compile-time check: ProviderAdapter satisfies the service's provider port.
var _ wikisvc.MaintenanceProvider = (*ProviderAdapter)(nil)

// ProposeIngest maps the service's affected-page set to a provider
// WikiIngestRequest, calls the inner provider, and converts the returned
// patches back to service.PagePatch. A nil inner provider returns nil (no-op
// run, §4.1).
func (a *ProviderAdapter) ProposeIngest(ctx context.Context, spaceID uuid.UUID, _ string, pages []wikisvc.AffectedPage) ([]wikisvc.PagePatch, error) {
	if a.Inner == nil {
		return nil, nil
	}
	req := wikiprovider.WikiIngestRequest{
		WikiSpaceID:    spaceID,
		Schema:         a.Schema,
		AffectedPages:  toProviderPageRefs(pages),
		SourceVersions: collectSourceVersions(pages),
	}
	patches, err := a.Inner.ProposeIngest(ctx, a.capability(spaceID), req)
	if err != nil {
		return nil, err
	}
	return toSvcPatches(patches), nil
}

// ProposeAnswer maps an explicit settle-answer request to a provider
// WikiAnswerRequest (§4.3 query_file).
func (a *ProviderAdapter) ProposeAnswer(ctx context.Context, spaceID uuid.UUID, pageKey string, answerRef map[string]any, pages []wikisvc.AffectedPage) ([]wikisvc.PagePatch, error) {
	if a.Inner == nil {
		return nil, nil
	}
	var rawAnswer json.RawMessage
	if len(answerRef) > 0 {
		if b, err := json.Marshal(answerRef); err == nil {
			rawAnswer = b
		}
	}
	req := wikiprovider.WikiAnswerRequest{
		WikiSpaceID:    spaceID,
		Schema:         a.Schema,
		PageKey:        pageKey,
		AnswerRef:      rawAnswer,
		SourceVersions: collectSourceVersions(pages),
	}
	patches, err := a.Inner.ProposeAnswer(ctx, a.capability(spaceID), req)
	if err != nil {
		return nil, err
	}
	return toSvcPatches(patches), nil
}

// capability builds the §10.4 Capability envelope. MaxReadBytes / MaxReadPages
// use generous defaults so the NoopProvider path is observable; a real model
// adapter trims these from the acting principal's RBAC budget.
func (a *ProviderAdapter) capability(spaceID uuid.UUID) wikiprovider.Capability {
	return wikiprovider.Capability{
		WorkspaceID:   a.WorkspaceID,
		MaxReadBytes:  1 << 20, // 1 MiB
		MaxReadPages:  256,
	}
}

// toProviderPageRefs converts the service's AffectedPage slice to the provider's
// PageRef slice. For a locked page only the page_key + current version summary
// is carried (§4.4 point 1) — the provider never receives locked-page body
// text. The summary here is a minimal {page_key, page_kind} envelope; the body
// is intentionally absent.
func toProviderPageRefs(pages []wikisvc.AffectedPage) []wikiprovider.PageRef {
	out := make([]wikiprovider.PageRef, 0, len(pages))
	for _, p := range pages {
		summary, _ := json.Marshal(map[string]any{
			"page_key":  p.PageKey,
			"page_kind": p.PageKind,
		})
		out = append(out, wikiprovider.PageRef{
			PageKey:           p.PageKey,
			PageKind:          p.PageKind,
			AutomationState:   string(p.AutomationState),
			CurrentVersionID:  p.CurrentVersionID,
			Summary:           summary,
		})
	}
	return out
}

// collectSourceVersions flattens the per-page authorized source version sets
// into the single set the provider may read for the run (§8.1 — fixed before
// the call). Dedup is by (source_asset_id, source_asset_version_id).
func collectSourceVersions(pages []wikisvc.AffectedPage) []wikiprovider.SourceVersionRef {
	seen := make(map[[2]uuid.UUID]bool)
	out := make([]wikiprovider.SourceVersionRef, 0)
	for _, p := range pages {
		for _, sv := range p.SourceVersions {
			key := [2]uuid.UUID{sv.SourceAssetID, sv.SourceAssetVersionID}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, wikiprovider.SourceVersionRef{
				SourceAssetID:        sv.SourceAssetID,
				SourceAssetVersionID: sv.SourceAssetVersionID,
				ContributionHash:     sv.ContributionHash,
			})
		}
	}
	return out
}

// toSvcPatches converts provider.PagePatch → service.PagePatch.
func toSvcPatches(pp []wikiprovider.PagePatch) []wikisvc.PagePatch {
	out := make([]wikisvc.PagePatch, 0, len(pp))
	for _, p := range pp {
		sv := make([]wikisvc.SourceVersionRef, 0, len(p.SourceVersions))
		for _, s := range p.SourceVersions {
			sv = append(sv, wikisvc.SourceVersionRef{
				SourceAssetID:        s.SourceAssetID,
				SourceAssetVersionID: s.SourceAssetVersionID,
				ContributionHash:     s.ContributionHash,
			})
		}
		rs := make([]wikisvc.RelationSuggestion, 0, len(p.RelationSuggestions))
		for _, r := range p.RelationSuggestions {
			rs = append(rs, wikisvc.RelationSuggestion{
				Kind: r.Kind, ToAssetID: r.ToAssetID, ToVersionID: r.ToVersionID,
			})
		}
		out = append(out, wikisvc.PagePatch{
			PageKey:             p.PageKey,
			ExpectedVersionID:   p.ExpectedVersionID,
			Action:              p.Action,
			ContentHash:          p.ContentHash,
			SourceVersions:       sv,
			RelationSuggestions: rs,
		})
	}
	return out
}
