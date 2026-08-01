// Package search implements hybrid retrieval (05-rag-pipeline-design.md §6):
// Dense (Qdrant) + BM25 (PostgreSQL FTS) fused by Reciprocal Rank Fusion, with
// optional Cross-Encoder reranking (P1). RBAC is a hard filter on both paths
// and is defensively re-checked before returning (existence never leaks).
package search

import (
	"sort"

	"github.com/lynn901/mora/internal/module/rag"
)

// rrfK is the RRF constant (05 §6.4, default 60).
const rrfK = 60

// candidate is a fused retrieval candidate keyed by (document_id, chunk_index).
type candidate struct {
	DocumentID  string
	Title       string
	ChunkText   string
	ChunkIndex  int
	SectionPath string
	WorkspaceID string
	DenseScore  float32
	BM25Score   float32
	FusedScore  float32
	RerankScore float32
}

// rrfFuse merges Dense and BM25 results by Reciprocal Rank Fusion.
// score = Σ 1/(k + rank), rank starts at 1 for the top result of each path.
func rrfFuse(dense []rag.VectorHit, bm25 []rag.FTSHit) []candidate {
	byKey := make(map[string]*candidate)

	addDense := func(rank int, h rag.VectorHit) {
		key := h.Payload.DocumentID + "|" + itoa(h.Payload.ChunkIndex)
		c, ok := byKey[key]
		if !ok {
			c = &candidate{
				DocumentID:  h.Payload.DocumentID,
				ChunkText:   h.Payload.ChunkText,
				ChunkIndex:  h.Payload.ChunkIndex,
				SectionPath: h.Payload.SectionPath,
				WorkspaceID: h.Payload.WorkspaceID,
			}
			byKey[key] = c
		}
		c.DenseScore = h.Score
		c.FusedScore += 1.0 / float32(rrfK+rank)
	}
	for i, h := range dense {
		addDense(i+1, h)
	}

	addBM25 := func(rank int, h rag.FTSHit) {
		key := h.DocumentID + "|" + itoa(h.ChunkIndex)
		c, ok := byKey[key]
		if !ok {
			c = &candidate{
				DocumentID:  h.DocumentID,
				ChunkText:   h.ChunkText,
				ChunkIndex:  h.ChunkIndex,
				WorkspaceID: h.WorkspaceID,
			}
			byKey[key] = c
		}
		if c.Title == "" {
			c.Title = h.Title
		}
		c.BM25Score = h.Score
		c.FusedScore += 1.0 / float32(rrfK+rank)
	}
	for i, h := range bm25 {
		addBM25(i+1, h)
	}

	out := make([]candidate, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FusedScore > out[j].FusedScore })
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
