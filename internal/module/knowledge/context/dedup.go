// Package context — cross-type dedup + conflict retention (design-docs/19 §7.2,
// D7).
//
// After the parallel fan-out (§7.1 step 4) and before scoring (step 7), the
// Broker collapses the merged candidate list by dedup key WHILE keeping
// conflict pairs side-by-side instead of picking one (§7.2). This file is
// step 6 of the §7.1 pipeline.
//
// Dedup keys (§7.2), in priority order:
//  1. asset_id — same asset, different versions → keep the newest ready.
//  2. content_hash — same content, different assets → keep one.
//
// Conflict retention (§7.2): a candidate that carries a conflict signal the
// policy declares must-surface (policy.ConflictsToSurface) is PINNED — it
// survives dedup. When two same-asset candidates are both pinned (a conflict
// pair on one asset — e.g. two versions that contradict), both are kept so the
// pair is presented side-by-side (§7.2 "两方并列，不合并、不择一"). The conflict
// signal is the UNION of:
//   - Relations[i].RelationType (Phase 1 directed edges: contradicts /
//     supersedes, with a TargetID — §7.2), and
//   - ConflictTags (candidate-self labels: old_spec / impl_drift /
//     version_mismatch / …, no counterpart — §7.2).
//
// Exclusion (§7.2): BEFORE dedup, candidates carrying an exclude_when tag the
// policy declares (deprecated / version_mismatch by default) are dropped —
// unless that same tag is also in must_surface_conflicts (the §7.2 mutual-
// exclusion: a tag cannot be both excluded and surfaced; the PUT handler
// rejects that config, and DedupAndKeepConflicts defends in depth by letting
// must_surface win). Authorization is a PRE-FETCH hard filter (§3.2), NOT an
// exclude_when tag — it never reaches this pass.
package context

import (
	"sort"

	"github.com/google/uuid"
)

// DedupAndKeepConflicts collapses the merged candidate list by asset_id →
// content_hash, keeping conflict pairs side-by-side when the policy declares
// the conflict type must-surface (§7.2 step 6). It is a pure function over
// the candidate slice + the policy; no IO, no authz (authz is the pre-fetch
// hard filter and the post-fetch batch check, §3.2 — neither lives here).
//
// Order of operations (§7.2):
//  1. excludeByPolicy — drop deprecated/version_mismatch (the policy's
//     ExcludeWhen), unless the tag is also must-surface (§7.2 mutual-exclusion
//     defense; the PUT handler rejects that config, this is belt+suspenders).
//  2. Pin conflict carriers — a candidate whose conflict-signal union
//     intersects policy.ConflictsToSurface() is pinned; it survives dedup.
//  3. Dedup by asset_id — same asset, keep the newest ready (Freshness desc,
//     then Score desc as the tiebreak); a pinned candidate is never the drop,
//     and a same-asset pinned pair is kept side-by-side.
//  4. Dedup by content_hash — same content, different assets → keep one; a
//     pinned candidate is never the drop.
//
// The returned slice is stable-sorted by Freshness desc then Score desc so the
// policy.Score step (§7.1 step 7) re-scores a deterministic input order.
func DedupAndKeepConflicts(candidates []KnowledgeCandidate, policy AuthorityPolicy) []KnowledgeCandidate {
	if len(candidates) == 0 {
		return nil
	}

	var surface map[string]bool
	var exclude map[string]bool
	if policy != nil {
		surface = conflictTypeSet(policy.ConflictsToSurface())
		exclude = excludeSet(policyExcludeWhen(policy), surface)
	}

	// Step 1: exclude_by_policy (§7.2). Drops deprecated/version_mismatch etc.
	// unless the tag is also must-surface (mutual-exclusion defense).
	kept := make([]KnowledgeCandidate, 0, len(candidates))
	for _, c := range candidates {
		if shouldExclude(c, exclude) {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil
	}

	// Step 2: pin conflict carriers (§7.2). A candidate whose conflict-signal
	// union intersects ConflictsToSurface is pinned — it survives dedup.
	pinned := make(map[uuid.UUID]bool, len(kept))
	for _, c := range kept {
		if hasConflictToSurface(c, surface) {
			pinned[c.AssetID] = true
		}
	}

	// Step 3: dedup by asset_id. Same asset → keep the newest ready, UNLESS a
	// member is pinned (then keep every pinned member side-by-side, §7.2).
	kept = dedupByAssetID(kept, pinned, surface)

	// Step 4: dedup by content_hash — same content, different assets → keep
	// one (the higher Freshness/Score). Pinned candidates survive.
	kept = dedupByContentHash(kept, pinned)

	// Stable ordering: Freshness desc, then Score desc. policy.Score re-scores
	// on top of this; a deterministic input keeps the stable sort meaningful
	// (§5.2 ties preserve input order).
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Freshness != kept[j].Freshness {
			return kept[i].Freshness > kept[j].Freshness
		}
		return kept[i].Score > kept[j].Score
	})
	return kept
}

// policyExcludeWhen reads the policy's ExcludeWhen list. The built-in policy
// carries it on cfg; the AuthorityPolicy port does not expose it directly, so
// we type-assert to *builtInPolicy. An unknown policy implementation yields
// nil (no exclusion) — the safe default (no candidate dropped).
func policyExcludeWhen(policy AuthorityPolicy) []string {
	if p, ok := policy.(*builtInPolicy); ok {
		return p.cfg.ExcludeWhen
	}
	return nil
}

// excludeSet builds the set of tags that trigger exclusion, MINUS any tag that
// is also in surface (§7.2 mutual-exclusion: a tag cannot be both excluded and
// surfaced — must_surface wins so the conflict is never dropped before dedup).
func excludeSet(excludeWhen []string, surface map[string]bool) map[string]bool {
	if len(excludeWhen) == 0 {
		return nil
	}
	out := make(map[string]bool, len(excludeWhen))
	for _, tag := range excludeWhen {
		if surface[tag] {
			continue // must_surface wins (§7.2 mutual-exclusion defense).
		}
		out[tag] = true
	}
	return out
}

// shouldExclude reports whether the candidate carries an exclude tag (in its
// ConflictTags). Relations are directed edges to another asset, not self-state
// — they are not exclusion signals (§7.2 exclude_when is candidate-self).
func shouldExclude(c KnowledgeCandidate, exclude map[string]bool) bool {
	if len(exclude) == 0 || len(c.ConflictTags) == 0 {
		return false
	}
	for _, tag := range c.ConflictTags {
		if exclude[tag] {
			return true
		}
	}
	return false
}

// hasConflictToSurface reports whether the candidate's conflict-signal union
// intersects the policy's must-surface set (§7.2). The union is Relations[i].
// RelationType ∪ ConflictTags.
func hasConflictToSurface(c KnowledgeCandidate, surface map[string]bool) bool {
	if len(surface) == 0 {
		return false
	}
	for _, r := range c.Relations {
		if surface[r.RelationType] {
			return true
		}
	}
	for _, tag := range c.ConflictTags {
		if surface[tag] {
			return true
		}
	}
	return false
}

// dedupByAssetID collapses same-asset candidates. Same asset → keep the newest
// ready (Freshness desc, then Score desc), UNLESS a member is pinned: every
// pinned member of a same-asset group is kept so a conflict pair is presented
// side-by-side (§7.2). A non-pinned twin of a pinned candidate is dropped (the
// pinned one is the conflict carrier; the twin is a redundant fetch).
func dedupByAssetID(candidates []KnowledgeCandidate, pinned map[uuid.UUID]bool, surface map[string]bool) []KnowledgeCandidate {
	if len(candidates) <= 1 {
		return candidates
	}
	byAsset := make(map[uuid.UUID][]KnowledgeCandidate, len(candidates))
	order := make([]uuid.UUID, 0, len(candidates))
	for _, c := range candidates {
		if _, seen := byAsset[c.AssetID]; !seen {
			order = append(order, c.AssetID)
		}
		byAsset[c.AssetID] = append(byAsset[c.AssetID], c)
	}
	out := make([]KnowledgeCandidate, 0, len(candidates))
	for _, aid := range order {
		group := byAsset[aid]
		if len(group) == 1 {
			out = append(out, group[0])
			continue
		}
		// Keep every member that carries a surfaceable conflict (the pair is
		// side-by-side). If none carries one (pinned by asset only — cannot
		// happen since pinning is by conflict signal, but defend in depth),
		// keep the newest ready so the asset is not lost.
		anyKept := false
		for _, c := range group {
			if hasConflictToSurface(c, surface) {
				out = append(out, c)
				anyKept = true
			}
		}
		if !anyKept {
			out = append(out, newestReady(group))
		}
	}
	return out
}

// dedupByContentHash collapses same-content-different-asset candidates to one
// (§7.2 content_hash key). A pinned candidate is never the drop. Candidates
// with an empty ContentHash are never deduped here (§7.2 — the engine may not
// compute one).
func dedupByContentHash(candidates []KnowledgeCandidate, pinned map[uuid.UUID]bool) []KnowledgeCandidate {
	if len(candidates) <= 1 {
		return candidates
	}
	byHash := make(map[string][]KnowledgeCandidate, len(candidates))
	for _, c := range candidates {
		if c.ContentHash == "" {
			continue
		}
		byHash[c.ContentHash] = append(byHash[c.ContentHash], c)
	}
	if len(byHash) == 0 {
		return candidates
	}
	keep := make(map[uuid.UUID]bool, len(candidates))
	for _, c := range candidates {
		if c.ContentHash == "" {
			keep[c.AssetID] = true // no hash → not a dedup target
			continue
		}
		group := byHash[c.ContentHash]
		if len(group) == 1 {
			keep[c.AssetID] = true
			continue
		}
	}
	for _, group := range byHash {
		if len(group) == 1 {
			continue
		}
		// Pinned members survive; if none pinned, keep the newest ready.
		anyPinned := false
		for _, c := range group {
			if pinned[c.AssetID] {
				keep[c.AssetID] = true
				anyPinned = true
			}
		}
		if !anyPinned {
			keep[newestReady(group).AssetID] = true
		}
	}
	out := make([]KnowledgeCandidate, 0, len(candidates))
	for _, c := range candidates {
		if keep[c.AssetID] {
			out = append(out, c)
		}
	}
	return out
}

// newestReady returns the candidate with the highest Freshness (then Score)
// from a same-key group (§7.2 "同资产不同版本取最新 ready"). candidateBetter(a,
// b) reports "a is better than b", so we replace best only when the challenger
// beats it — inverting that (replacing best when best is already better) would
// keep the oldest version, the §7.2 violation this guard prevents.
func newestReady(group []KnowledgeCandidate) KnowledgeCandidate {
	best := group[0]
	for _, c := range group[1:] {
		if candidateBetter(c, best) {
			best = c
		}
	}
	return best
}

// candidateBetter reports whether cur is strictly better than cand (Freshness
// desc, then Score desc). Used to pick the newest ready version.
func candidateBetter(cur, cand KnowledgeCandidate) bool {
	if cur.Freshness != cand.Freshness {
		return cur.Freshness > cand.Freshness
	}
	return cur.Score > cand.Score
}

// conflictTypeSet converts a conflict-type list to a set for O(1) lookup.
func conflictTypeSet(types []string) map[string]bool {
	if len(types) == 0 {
		return nil
	}
	out := make(map[string]bool, len(types))
	for _, t := range types {
		out[t] = true
	}
	return out
}
