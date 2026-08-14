// Package index implements the deterministic Wiki index/log Document rebuild
// (design-docs/16 §5). The index Document is the Space's table-of-contents
// page, rebuilt deterministically from the set of published pages — stable
// ordering (page_kind, page_key) and a stable hash so the same published set
// always yields the same index version (幂等, decision D6). The log Document
// is append-only: each Run completion or review Decision appends a
// structured record; history entries are never rewritten (不可改写, §5.2).
//
// Both index and log are themselves document assets (asset_type='document',
// content_origin='system'); they reuse the existing Document FTS / Qdrant
// projection (§5.3). This package only computes the content + hash; the
// service/worker creates the knowledge_asset_versions row + projection job.
package index

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// PublishedPage is one entry in the deterministic index (§5.1). The stable
// hash inputs are page_key + current_version's content_hash.
type PublishedPage struct {
	PageKey      string `json:"page_key"`
	PageKind     string `json:"page_kind"`
	ContentHash  string `json:"content_hash"`
}

// IndexContent is the JSON-serializable body of the index Document. It is the
// sorted list of published pages; the same set → the same bytes → the same
// index_version_hash (§5.1 稳定哈希).
type IndexContent struct {
	Pages []IndexEntry `json:"pages"`
}

// IndexEntry is one row in the index Document.
type IndexEntry struct {
	PageKey     string `json:"page_key"`
	PageKind    string `json:"page_kind"`
	ContentHash string `json:"content_hash"`
}

// LogEntry is one append-only record in the log Document (§5.2). The log's
// content is a JSON array of these; rebuild only appends, never rewrites.
type LogEntry struct {
	RunID     uuid.UUID `json:"run_id,omitempty"`
	Trigger   string    `json:"trigger,omitempty"`
	PageKeys  []string  `json:"page_keys,omitempty"`
	Decision  string    `json:"decision,omitempty"`
	ActorType string    `json:"actor_type,omitempty"`
	ActorID   uuid.UUID `json:"actor_id,omitempty"`
	Timestamp string    `json:"timestamp"`
}

// BuildIndex computes the deterministic index content + hash for a set of
// published pages (§5.1). Pages are sorted by (page_kind, page_key) so the
// order is independent of generation time.
func BuildIndex(pages []PublishedPage) (content []byte, hash string, err error) {
	// Deterministic sort: (page_kind, page_key).
	sorted := make([]PublishedPage, len(pages))
	copy(sorted, pages)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].PageKind != sorted[j].PageKind {
			return sorted[i].PageKind < sorted[j].PageKind
		}
		return sorted[i].PageKey < sorted[j].PageKey
	})
	entries := make([]IndexEntry, 0, len(sorted))
	for _, p := range sorted {
		entries = append(entries, IndexEntry{
			PageKey: p.PageKey, PageKind: p.PageKind, ContentHash: p.ContentHash,
		})
	}
	body := IndexContent{Pages: entries}
	content, err = json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("index: marshal content: %w", err)
	}
	// Stable hash over the sorted (page_key, content_hash) pairs.
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintln(h, e.PageKey)
		fmt.Fprintln(h, e.ContentHash)
	}
	return content, fmt.Sprintf("%x", h.Sum(nil)), nil
}

// AppendLog computes the new log content by appending entry to the prior log
// body (§5.2 append-only). Prior may be nil (first entry) or a decoded array.
// The returned content is the full new array; the hash is over the appended
// entry's canonical bytes so the same Run/Decision yields the same log
// version (幂等).
func AppendLog(prior []byte, entry LogEntry) (content []byte, hash string, err error) {
	var entries []LogEntry
	if len(prior) > 0 {
		if err = json.Unmarshal(prior, &entries); err != nil {
			return nil, "", fmt.Errorf("index: unmarshal prior log: %w", err)
		}
	}
	entries = append(entries, entry)
	content, err = json.Marshal(entries)
	if err != nil {
		return nil, "", fmt.Errorf("index: marshal log: %w", err)
	}
	// Hash over the appended entry alone → same Run/Decision always lands the
	// same log version (dedupe). The log's array grows but each entry is a
	// distinct version input.
	eb, _ := json.Marshal(entry)
	h := sha256.New()
	h.Write(eb)
	return content, fmt.Sprintf("%x", h.Sum(nil)), nil
}

// HashMatches reports whether a candidate hash equals the existing index
// version hash — used to skip a no-op rebuild (§11 "index 重建抖动" mitigation).
func HashMatches(a, b string) bool { return a != "" && a == b }
