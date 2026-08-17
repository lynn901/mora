// Package dedup — inbox ordering (design-docs/18 §6.3).
//
// §6.3: the reviewer inbox is sorted so that evidence_missing units and
// contradicts suggestions surface first — those are the items that most need a
// human decision before they age into recall (a contradicts pair left pending
// keeps recall surfacing both sides; an evidence_missing unit must not age
// into high-authority recall, §8.4). The sort is stable so the created_at
// ordering the repo returns is preserved within each bucket.
package dedup

import (
	"sort"

	"github.com/lynn901/mora/internal/domain"
)

// sortInbox orders items + suggestions by review priority (§6.3):
//
//   - items: evidence_missing candidates first, then the rest, each bucket
//     preserving the repo's created_at order.
//   - suggestions: contradicts first, then duplicate/extends, each bucket by
//     created_at. contradicts is the higher priority because an unconfirmed
//     contradicts keeps recall returning both sides of a conflict (§8.3).
//
// The sort is in-place + stable. Empty slices are a no-op.
func sortInbox(items []InboxItem, suggestions []domain.MemoryDedupSuggestion) {
	if len(items) > 1 {
		sort.SliceStable(items, func(i, j int) bool {
			ai, aj := items[i].Unit.EvidenceMissing, items[j].Unit.EvidenceMissing
			if ai != aj {
				return ai // evidence_missing first
			}
			return items[i].Unit.CreatedAt.Before(items[j].Unit.CreatedAt)
		})
	}
	if len(suggestions) > 1 {
		sort.SliceStable(suggestions, func(i, j int) bool {
			bi, bj := suggestionPriority(suggestions[i]), suggestionPriority(suggestions[j])
			if bi != bj {
				return bi < bj
			}
			return suggestions[i].CreatedAt.Before(suggestions[j].CreatedAt)
		})
	}
}

// suggestionPriority ranks a suggestion for inbox ordering (§6.3). contradicts
// (1) before duplicate/extends (2); a lower number sorts first. The State is
// ignored — ListPending only returns pending suggestions, but the helper is
// defensive against a non-pending row slipping through (it keeps its bucket).
func suggestionPriority(s domain.MemoryDedupSuggestion) int {
	switch s.SuggestionType {
	case domain.DedupContradicts:
		return 1
	case domain.DedupDuplicate, domain.DedupExtends:
		return 2
	default:
		return 3
	}
}
