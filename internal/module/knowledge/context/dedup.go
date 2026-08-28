// dedup.go — DedupAndKeepConflicts 签名（§7.2 去重 + 冲突保留 D7）。
//
// Dedup keys (§7.2):
//   - asset_id — same asset different version: keep the latest ready.
//   - content_hash — same content different asset: keep one but record the
//     co-source (do not double-count).
//
// Conflict preservation (§7.2 / §5.1 must_surface_conflicts): if a
// candidate's Relations contains contradicts/supersedes AND the policy lists
// that conflict type in ConflictsToSurface, BOTH sides are kept side-by-side
// — not merged, not picked-between (11 §7.2: never silently pick one answer).
//
// Exclusion (§7.2): deprecated / expired / version-mismatched assets do NOT
// enter the result by default. Permission is a hard pre-filter, NOT a
// multiplicative score factor (§7.2 — the authz two-stage gate already
// removed them).
//
// Implementation lands in a follow-up sub-task; the signature is fixed here.

// DedupAndKeepConflicts deduplicates candidates by asset_id / content_hash BUT
// keeps conflict relations (contradicts/old_spec/impl_drift) side by side
// instead of picking one (12 §9.2 step 6, §9.5 must_surface_conflicts).
package contextbroker

func DedupAndKeepConflicts(candidates []KnowledgeCandidate, policy AuthorityPolicy) []KnowledgeCandidate {
	// TODO: §7.2 — dedup by asset_id (latest ready) → content_hash (record
	// co-source) → keep both sides of any policy-must-surface conflict.
	return candidates
}
