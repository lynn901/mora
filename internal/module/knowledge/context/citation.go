package contextbroker

// citation.go — CitationBuilder 实现（§8.2）。
//
// The Citation is the traceable reference a candidate carries (11 §7.4 /
// §8.1). The type adapters (candidate.go CandidateFromMemory/Document/Code/
// Skill) pre-fill the fields the source engine already has; the
// CitationBuilder finalizes them AFTER the final authorization pass (§7.1
// step 9 / §8.2), mapping the per-type sub-structures the candidates already
// carry — NO re-resolution, only field mapping and format normalization
// (§8.2). ProjectionRef stays internal-diagnostic-only and is never returned
// to the Agent (§8.2, inherited from the Phase 4 recall constraint).
//
// The Citation struct itself is defined in candidate.go (the D2 unified shape);
// this file holds only the Builder.

// CitationBuilder finalizes citations after authorization (§8.2). It maps the
// per-type sub-structures candidates already carry — it does NOT re-resolve
// (§8.2). ProjectionRef stays internal-diagnostic-only and is never returned
// to the Agent (§8.2 / Phase 4 recall constraint).
//
// The default implementation (denormalizingCitationBuilder) is a pure,
// allocation-bounded mapper: it guarantees each returned candidate's Citation
// is self-contained (AssetID/AssetType/Authority/Confidence mirror the
// candidate) and that no ProjectionRef-equivalent internal field leaks. It is
// the LAST step before the response is serialized, so it is the guard that
// enforces "ProjectionRef not returned" even if a future adapter accidentally
// introduces one.
type CitationBuilder interface {
	Build(candidates []KnowledgeCandidate) []KnowledgeCandidate
}

// denormalizingCitationBuilder is the default CitationBuilder (§8.2). It is
// stateless; Build is safe to call concurrently. It does NOT touch the network
// or the DB — it only normalizes fields the candidates already carry.
type denormalizingCitationBuilder struct{}

// NewCitationBuilder returns the default §8.2 citation builder.
func NewCitationBuilder() CitationBuilder { return &denormalizingCitationBuilder{} }

// Build implements the §8.2 finalization:
//
//  1. For each candidate, ensure the Citation is SELF-CONTAINED for
//     audit/logging (§9.3): AssetID, AssetType, Authority, and Confidence on
//     the Citation mirror the candidate's own fields. The adapters already do
//     this at conversion time; the builder re-asserts it so a candidate that
//     traveled through dedup/budget (which operate on the candidate, not the
//     citation) cannot end up with a stale-mirrored citation.
//  2. VersionOrRevision is left as the adapter set it (document=version_id,
//     code=commit, memory=evidence_id, skill=package_version) — the builder
//     does NOT re-resolve the anchor from the engine (§8.2 "不重新解析").
//  3. SourceRef / Locator are preserved as-is (already sanitized by the
//     adapter; the builder does not re-sanitize, only passes through).
//  4. UpdatedAt: left zero when the source did not resolve it (omitempty keeps
//     it out of the JSON). The builder does not synthesize a timestamp — that
//     would be a re-resolution, forbidden by §8.2.
//  5. ProjectionRef: the Citation struct has no such field, so there is nothing
//     to strip; the builder is the guard that FAILS compilation if a future
//     edit adds one (the ProjectionRefNotReturned test below encodes this).
//
// Build returns the same slice (in place) — no allocation when no candidate
// needs a fixup, minimizing the cost on the hot path.
func (b *denormalizingCitationBuilder) Build(candidates []KnowledgeCandidate) []KnowledgeCandidate {
	if len(candidates) == 0 {
		return candidates
	}
	for i := range candidates {
		c := &candidates[i]
		cit := &c.Citation
		// §8.2 denormalize: mirror the candidate's authoritative fields onto the
		// citation so a citation is self-contained for audit even if read in
		// isolation from the candidate. The adapters do this at conversion; we
		// re-assert here as the post-authorization finalization gate.
		cit.AssetID = c.AssetID
		cit.AssetType = c.AssetType
		cit.Authority = c.Authority
		cit.Confidence = c.Confidence
	}
	return candidates
}
