// Package context — Citation Builder (design-docs/19 §8, D9).
//
// The CitationBuilder completes the traceable citation on each selected
// candidate AFTER the final authorization pass (§8.2). It does NOT re-parse
// the source engines — it reuses the sub-structures each type adapter already
// carried onto the candidate (memory evidence locator, code file:line, document
// block_id, skill resource path), mapping them onto the unified Citation shape
// (§8.1) and filling the fields only the post-authz layer can resolve:
// source_ref (sanitized), version_or_revision (the version anchor),
// updated_at (the version's last-update time), and locator.
//
// ProjectionRef is internal-diagnostic only and is NOT on the Citation struct
// (§8.2, inheriting the memory recall constraint). The CitationBuilder never
// adds it — there is no field to populate — so it cannot leak to the Agent.
//
// This file is the Builder + the CitationMetaLookup port (the post-authz field
// source for version/updated_at). The Broker (YS-209) calls Build as step 9 of
// the §7.1 pipeline, after Budgeter.Select (step 8).
package context

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// CitationBuilder completes the citation fields after the final authorization
// pass (§8.2). It is the step-9 component of the §7.1 pipeline.
type CitationBuilder interface {
	// Build completes the Citation on each selected candidate. It reuses the
	// candidate's carried sub-structures (no re-parse, §8.2) and fills
	// source_ref / version_or_revision / updated_at / locator from the
	// post-authz CitationMetaLookup. The returned candidates carry their
	// completed Citations; ProjectionRef is not present (§8.2).
	Build(ctx context.Context, selected []KnowledgeCandidate) []KnowledgeCandidate
}

// CitationMeta is the post-authz version + freshness meta for one asset
// version (§8.2). The CitationMetaLookup resolves this from the authoritative
// version tables (knowledge_asset_versions / memory_evidence / skill package
// versions / codegraph active graph) — the Broker holds the allowed-asset set
// after the batch post-check, so the lookup is post-authz by construction.
//
// Fields are pointers so a nil/zero value means "the source did not resolve
// it" (§8.1 — the CitationBuilder leaves the field unset rather than inventing
// a value). UpdatedAt is a value (zero = unset) so the JSON omitempty on
// Citation.UpdatedAt hides it.
type CitationMeta struct {
	AssetID   uuid.UUID
	AssetType domain.AssetType
	// SourceRef is the sanitized source name/url (no credentials, §8.1).
	SourceRef string
	// VersionOrRevision is the version anchor: document=version_id, codebase=
	// commit, memory=evidence_id, skill=package_version (§8.1).
	VersionOrRevision string
	// UpdatedAt is the asset/version's last-update time. Zero when unresolved.
	UpdatedAt time.Time
	// Locator is the precise position the source engine already carried
	// (document block_id, code file:line, memory conversation message, skill
	// resource path). The Builder merges this onto the candidate's existing
	// locator without dropping keys the adapter already set (§8.2 no re-parse).
	Locator map[string]any
}

// CitationMetaLookup is the post-authz field source (§8.2). The Broker calls
// it with the selected (post-check-passed) asset+version pairs; it returns the
// version anchor, updated_at, and sanitized source_ref the CitationBuilder
// completes the citation with. A missing meta for an asset means the source
// engine did not resolve a version (the candidate keeps its adapter-supplied
// citation, just without the post-authz completion — §8.1 "unversioned").
//
// The lookup is the only IO the CitationBuilder does; it is post-authz by
// construction (the Broker only passes post-check-passed asset IDs, §7.1
// step 5), so the Builder cannot leak existence of an unauthorized asset.
type CitationMetaLookup interface {
	// Lookup returns the post-authz meta for each (asset, version) pair. A pair
	// with no resolvable version yields a zero CitationMeta (AssetID set, the
	// rest zero-value); the Builder leaves those citation fields unset. The
	// returned slice is keyed by asset_id for O(1) lookup by the Builder.
	Lookup(ctx context.Context, workspaceID uuid.UUID, keys []CitationKey) (map[uuid.UUID]CitationMeta, error)
}

// CitationKey is one (asset, version) pair the Broker asks the lookup to
// resolve (§8.2). AssetVersion may be nil (unversioned candidate); the lookup
// resolves the current/latest version anchor in that case.
type CitationKey struct {
	AssetID      uuid.UUID
	AssetVersion *uuid.UUID
}

// citationBuilder is the default CitationBuilder (§8.2).
type citationBuilder struct {
	lookup CitationMetaLookup
}

// NewCitationBuilder wires the post-authz meta lookup. A nil lookup makes the
// Builder a pure field-mapper: it completes the citation from the candidate's
// carried sub-structures only (no version/updated_at completion). The wiring
// layer passes the real lookup; tests pass nil to assert the no-IO path.
func NewCitationBuilder(lookup CitationMetaLookup) CitationBuilder {
	return &citationBuilder{lookup: lookup}
}

// Build implements §8.2. For each selected candidate it:
//
//  1. Carries forward the adapter-supplied citation fields (source_ref,
//     version_or_revision, locator) — NO re-parse (§8.2). The adapter already
//     filled what the source engine carried.
//  2. Overlays the post-authz CitationMeta for the missing/completable fields:
//     source_ref (sanitized), version_or_revision (version anchor), updated_at,
//     locator (merged — the lookup's keys win on conflict but adapter keys the
//     lookup did not set are preserved).
//  3. Denormalizes AssetID/AssetType/Authority/Confidence so the citation is
//     self-contained for audit (§9.3).
//  4. Does NOT add ProjectionRef — there is no such field on Citation (§8.2
//     internal-only; the struct cannot carry it, so it cannot leak).
//
// The returned slice is the same candidates with completed Citations; order is
// preserved.
func (b *citationBuilder) Build(ctx context.Context, selected []KnowledgeCandidate) []KnowledgeCandidate {
	if len(selected) == 0 {
		return selected
	}
	// Build the lookup keys once (post-authz by construction — the Broker only
	// passes post-check-passed candidates, §7.1 step 5).
	keys := make([]CitationKey, 0, len(selected))
	for _, c := range selected {
		keys = append(keys, CitationKey{AssetID: c.AssetID, AssetVersion: c.AssetVersion})
	}

	// No lookup → pure field-map from the candidate's carried citation. The
	// adapter already filled source_ref/version_or_revision/locator; the
	// Builder just ensures the denormalized identity fields are set (§9.3).
	if b.lookup == nil {
		out := make([]KnowledgeCandidate, len(selected))
		for i, c := range selected {
			out[i] = completeFromCandidate(c)
		}
		return out
	}

	// workspaceID is not on the candidate; the Broker passes it via ctx in the
	// real wiring (YS-209). The lookup here keys by asset_id, so an empty
	// workspace is fine for the per-asset resolution — the real lookup enforces
	// the workspace scope. Nil-map (lookup error) degrades to the carried cite.
	meta, err := b.lookup.Lookup(ctx, uuid.Nil, keys)
	if err != nil || meta == nil {
		meta = map[uuid.UUID]CitationMeta{}
	}

	out := make([]KnowledgeCandidate, len(selected))
	for i, c := range selected {
		out[i] = b.completeOne(c, meta)
	}
	return out
}

// completeOne completes one candidate's citation from its carried sub-structure
// + the post-authz meta (§8.2). Carried fields are preserved; meta fills the
// rest. Locator is MERGED (lookup keys win, adapter keys preserved) so the
// Builder never drops a locator key the adapter set (§8.2 no re-parse).
func (b *citationBuilder) completeOne(c KnowledgeCandidate, meta map[uuid.UUID]CitationMeta) KnowledgeCandidate {
	// Start from the candidate's carried citation (adapter-supplied).
	cite := completeFromCandidate(c).Citation

	m, ok := meta[c.AssetID]
	if ok {
		// Post-authz completion: fill only what the meta resolved (§8.1 —
		// unresolved fields stay zero/empty, never invented).
		if m.SourceRef != "" {
			cite.SourceRef = m.SourceRef
		}
		if m.VersionOrRevision != "" {
			cite.VersionOrRevision = m.VersionOrRevision
		}
		if !m.UpdatedAt.IsZero() {
			cite.UpdatedAt = m.UpdatedAt
		}
		if len(m.Locator) > 0 {
			cite.Locator = mergeLocator(cite.Locator, m.Locator)
		}
	}

	out := c
	out.Citation = cite
	return out
}

// completeFromCandidate fills the denormalized identity/authority/confidence
// fields on the citation from the candidate, WITHOUT touching the
// adapter-supplied source_ref/version_or_revision/locator (§9.3 self-contained
// for audit, §8.2 no re-parse). This is the no-lookup path and the base for the
// lookup path.
func completeFromCandidate(c KnowledgeCandidate) KnowledgeCandidate {
	cite := c.Citation
	// Denormalize identity so the citation is self-contained (§9.3).
	cite.AssetID = c.AssetID
	cite.AssetType = c.AssetType
	cite.Authority = c.Authority
	cite.Confidence = c.Confidence
	// Locator: preserve the adapter-supplied locator (memory evidence locator,
	// code file:line, document block_id, skill resource path). The adapter
	// already set it; the Builder does not re-parse (§8.2).
	if cite.Locator == nil {
		cite.Locator = map[string]any{}
	}
	out := c
	out.Citation = cite
	return out
}

// mergeLocator merges lookup locator keys onto the carried locator, with the
// lookup winning on conflict BUT the carried keys the lookup did not set
// preserved (§8.2 no re-parse — the adapter's block_id/file:line/quote must
// survive even when the post-authz meta adds keys). Allocates a new map so the
// candidate's carried locator is not mutated.
func mergeLocator(carried, fromLookup map[string]any) map[string]any {
	out := make(map[string]any, len(carried)+len(fromLookup))
	for k, v := range carried {
		out[k] = v
	}
	for k, v := range fromLookup {
		out[k] = v // lookup wins on conflict
	}
	return out
}
