package version

// Package version implements document version diffing and rollback semantics
// (PRD F1.3, AC-6: 任意两版本 Diff 比对与一键回滚；回滚产生新版本).
//
// Diff is computed at the block level: blocks are matched by stable Block.ID
// when present, otherwise by position+type. Each result entry is one of
// added / removed / modified, carrying the before/after block content.

import (
	"github.com/wiki/wiki-backend/internal/domain"
)

// DiffEntry describes a single block-level change between two versions.
type DiffEntry struct {
	Type     string         `json:"type"` // added | removed | modified
	BlockID  string         `json:"block_id,omitempty"`
	From     *domain.Block  `json:"from,omitempty"`
	To       *domain.Block  `json:"to,omitempty"`
	Content  *domain.Block  `json:"content,omitempty"` // for added/removed
}

// Diff computes block-level changes from version `from` to version `to`.
func Diff(from, to []domain.Block) []DiffEntry {
	fa := indexByID(from)

	var entries []DiffEntry
	seen := make(map[string]bool)

	// Walk `to` blocks in order to detect additions and modifications.
	for i := range to {
		b := &to[i]
		key := b.ID
		if key == "" {
			key = posKey(b.Type, i)
		}
		seen[key] = true
		if old, ok := fa[key]; ok {
			if !blocksEqual(&old, b) {
				ob := old
				nb := *b
				entries = append(entries, DiffEntry{Type: "modified", BlockID: key, From: &ob, To: &nb})
			}
		} else {
			nb := *b
			entries = append(entries, DiffEntry{Type: "added", BlockID: key, Content: &nb})
		}
	}
	// Walk `from` blocks to detect removals.
	for i := range from {
		b := &from[i]
		key := b.ID
		if key == "" {
			key = posKey(b.Type, i)
		}
		if !seen[key] {
			ob := *b
			entries = append(entries, DiffEntry{Type: "removed", BlockID: key, Content: &ob})
		}
	}
	return entries
}

// Summary produces a short human-readable diff summary string.
func Summary(entries []DiffEntry) string {
	if len(entries) == 0 {
		return "no changes"
	}
	add, rem, mod := 0, 0, 0
	for _, e := range entries {
		switch e.Type {
		case "added":
			add++
		case "removed":
			rem++
		case "modified":
			mod++
		}
	}
	parts := []string{}
	if add > 0 {
		parts = append(parts, numword(add, "addition", "additions"))
	}
	if rem > 0 {
		parts = append(parts, numword(rem, "removal", "removals"))
	}
	if mod > 0 {
		parts = append(parts, numword(mod, "modification", "modifications"))
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// NextVersionNo computes the next version number for a rollback. Rollback
// produces a NEW version (never overwrites history): nextNo = max(existing)+1.
func NextVersionNo(existingMax int) int {
	if existingMax < 1 {
		return 1
	}
	return existingMax + 1
}

// --- helpers ---

func indexByID(blocks []domain.Block) map[string]domain.Block {
	m := make(map[string]domain.Block, len(blocks))
	for i := range blocks {
		b := &blocks[i]
		key := b.ID
		if key == "" {
			key = posKey(b.Type, i)
		}
		if _, ok := m[key]; !ok {
			m[key] = *b
		}
	}
	return m
}

func posKey(t domain.BlockType, i int) string {
	return string(t) + "#" + itoa(i)
}

func blocksEqual(a, b *domain.Block) bool {
	if a.Type != b.Type {
		return false
	}
	if a.Text != b.Text {
		return false
	}
	if len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		if !blocksEqual(&a.Content[i], &b.Content[i]) {
			return false
		}
	}
	if len(a.Attrs) != len(b.Attrs) {
		return false
	}
	for k, v := range a.Attrs {
		if bv, ok := b.Attrs[k]; !ok || !equalAny(v, bv) {
			return false
		}
	}
	return true
}

func equalAny(a, b any) bool {
	return a == b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func numword(n int, singular, plural string) string {
	w := singular
	if n != 1 {
		w = plural
	}
	return itoa(n) + " " + w
}
