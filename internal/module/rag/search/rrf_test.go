package search

import (
	"testing"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag"
)

func mkDense(docID string, chunkIdx int, score float32) rag.VectorHit {
	return rag.VectorHit{
		PointID: "p-" + docID,
		Score:   score,
		Payload: domain.ChunkMetadata{DocumentID: docID, ChunkIndex: chunkIdx, ChunkText: "d"},
	}
}

func mkBM25(docID string, chunkIdx int, score float32, title string) rag.FTSHit {
	return rag.FTSHit{DocumentID: docID, ChunkIndex: chunkIdx, Score: score, Title: title, ChunkText: "d"}
}

func TestRRF_FusionCombinesPaths(t *testing.T) {
	dense := []rag.VectorHit{
		mkDense("doc1", 0, 0.9),
		mkDense("doc2", 0, 0.8),
	}
	bm25 := []rag.FTSHit{
		mkBM25("doc2", 0, 5, "T2"),
		mkBM25("doc3", 0, 4, "T3"),
	}
	cands := rrfFuse(dense, bm25)
	// doc2 appears in both paths → highest fused score
	if cands[0].DocumentID != "doc2" {
		t.Errorf("expected doc2 first (both paths), got %s", cands[0].DocumentID)
	}
	if cands[0].FusedScore <= cands[1].FusedScore {
		t.Errorf("fused scores not descending: %v", cands)
	}
	// dense_score / bm25_score carried through
	byDoc := map[string]candidate{}
	for _, c := range cands {
		byDoc[c.DocumentID] = c
	}
	if byDoc["doc1"].DenseScore != 0.9 {
		t.Errorf("dense score not carried: %+v", byDoc["doc1"])
	}
	if byDoc["doc2"].BM25Score != 5 {
		t.Errorf("bm25 score not carried: %+v", byDoc["doc2"])
	}
	if byDoc["doc2"].Title != "T2" {
		t.Errorf("title not carried from bm25: %+v", byDoc["doc2"])
	}
}

func TestRRF_EmptyInputs(t *testing.T) {
	if c := rrfFuse(nil, nil); len(c) != 0 {
		t.Errorf("expected no candidates, got %d", len(c))
	}
}

func TestRRF_SameDocDifferentChunksNotMerged(t *testing.T) {
	dense := []rag.VectorHit{
		mkDense("doc1", 0, 0.9),
		mkDense("doc1", 1, 0.8),
	}
	cands := rrfFuse(dense, nil)
	if len(cands) != 2 {
		t.Errorf("different chunks of same doc must stay separate, got %d candidates", len(cands))
	}
}

func TestRecheckRBAC_DropsUnauthorized(t *testing.T) {
	dense := []rag.VectorHit{
		{Payload: domain.ChunkMetadata{DocumentID: "d1", ChunkIndex: 0, VisibleTo: []string{"user:alice"}}},
		{Payload: domain.ChunkMetadata{DocumentID: "d2", ChunkIndex: 0, VisibleTo: []string{"user:bob"}}},
	}
	cands := []candidate{
		{DocumentID: "d1", ChunkIndex: 0},
		{DocumentID: "d2", ChunkIndex: 0},
	}
	out := recheckRBAC(cands, dense, []string{"user:alice", "group:g1"})
	if len(out) != 1 || out[0].DocumentID != "d1" {
		t.Errorf("defense-in-depth filter must drop d2, got %+v", out)
	}
}
