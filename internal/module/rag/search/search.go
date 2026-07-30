package search

import (
	"context"
	"fmt"
	"strings"

	"github.com/wiki/wiki-backend/internal/module/rag"
)

// SearchRequest is the input to HybridSearcher.Search (API 04 §9).
type SearchRequest struct {
	Query       string
	UserID      string // RBAC principal
	WorkspaceID string // optional scope ("" = all visible)
	DirectoryID string // optional scope
	Tags        []string
	TopK        int // per-path recall (default 50)
	TopN        int // final result count (default 10)
	Rerank      bool
	Filters     map[string]any // reserved (updated_after, etc.)
}

// SearchHit is one result item (API 04 §9 response).
type SearchHit struct {
	DocumentID  string  `json:"document_id"`
	Title       string  `json:"title"`
	ChunkText   string  `json:"chunk_text"`
	ChunkIndex  int     `json:"chunk_index"`
	SectionPath string  `json:"section_path,omitempty"`
	Score       float32 `json:"score"`
	DenseScore  float32 `json:"dense_score"`
	BM25Score   float32 `json:"bm25_score"`
	RerankScore float32 `json:"rerank_score,omitempty"`
	WorkspaceID string  `json:"workspace_id"`
	SourceURL   string  `json:"source_url"`
}

// SearchResult is the search response payload.
type SearchResult struct {
	Items []SearchHit `json:"items"`
	Total int         `json:"total"`
}

// TitleLookup optionally hydrates document titles for Dense-only hits (docs that
// BM25 didn't return). If nil, such hits get an empty title. Supplied by YS-6.
type TitleLookup func(ctx context.Context, documentIDs []string) (map[string]string, error)

// HybridSearcher fuses Dense + BM25 (+ optional rerank) under RBAC hard filters.
type HybridSearcher struct {
	Models  rag.ModelStore
	Factory rag.ProviderFactory
	Vectors rag.VectorStore
	FTS     rag.FTSStore
	RBAC    rag.RBACResolver
	Titles  TitleLookup
	Logf    func(format string, args ...any)
}

func New(s HybridSearcher) *HybridSearcher {
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	return &s
}

// Search executes a hybrid query (05 §6.1). RBAC is enforced as a hard filter on
// both paths and re-checked before return; unauthorized chunks never surface.
func (s *HybridSearcher) Search(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	if strings.TrimSpace(req.Query) == "" {
		return &SearchResult{}, nil
	}
	topK := req.TopK
	if topK <= 0 {
		topK = 50
	}
	topN := req.TopN
	if topN <= 0 {
		topN = 10
	}

	// 1. RBAC envelope (hard filter subject set). Computed once, enforced on both paths.
	scope, err := s.RBAC.ViewerScope(ctx, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("rbac viewer scope: %w", err)
	}
	if len(scope.SubjectIDs) == 0 {
		// No read subjects ⇒ nothing visible. Existence not leaked.
		return &SearchResult{}, nil
	}
	ws := req.WorkspaceID
	if ws == "" && len(scope.WorkspaceIDs) == 1 {
		ws = scope.WorkspaceIDs[0]
	}

	model, err := s.Models.GetActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("get active model: %w", err)
	}
	coll := model.CollectionName()

	// 2. Dense (Qdrant) with visible_to MUST filter.
	var dense []rag.VectorHit
	prov, err := s.Factory.For(ctx, model)
	if err == nil {
		qv, qerr := prov.Embed(ctx, []string{req.Query}, model.InstructionQuery)
		if qerr == nil && len(qv) == 1 {
			dense, err = s.Vectors.SearchDense(ctx, rag.VectorSearchRequest{
				CollectionName: coll,
				Vector:         qv[0],
				TopK:           topK,
				WorkspaceID:    ws,
				VisibleTo:      scope.SubjectIDs, // HARD FILTER
				DirectoryID:    req.DirectoryID,
				Tags:           req.Tags,
			})
			if err != nil {
				s.Logf("rag/search: dense path failed: %v (degrading to BM25)", err)
				dense = nil
			}
		} else if qerr != nil {
			s.Logf("rag/search: query embed failed: %v (degrading to BM25)", qerr)
		}
	} else {
		s.Logf("rag/search: provider unavailable: %v (degrading to BM25)", err)
	}

	// 3. BM25 (PostgreSQL FTS) with RBAC SQL filter.
	bm25, err := s.FTS.SearchBM25(ctx, rag.FTSRequest{
		Query:       req.Query,
		TopK:        topK,
		WorkspaceID: ws,
		DirectoryID: req.DirectoryID,
		VisibleTo:   scope.SubjectIDs, // HARD FILTER
	})
	if err != nil {
		s.Logf("rag/search: bm25 path failed: %v", err)
		bm25 = nil
	}

	// 4. RRF fusion.
	cands := rrfFuse(dense, bm25)
	if len(cands) == 0 {
		return &SearchResult{}, nil
	}

	// hydrate titles for dense-only candidates (BM25 already filled some).
	if s.Titles != nil {
		ids := uniqueDocIDs(cands)
		if titles, terr := s.Titles(ctx, ids); terr == nil {
			for i := range cands {
				if cands[i].Title == "" {
					cands[i].Title = titles[cands[i].DocumentID]
				}
			}
		}
	}

	// 5. Optional rerank (P1). On failure, degrade to fused-score order.
	if req.Rerank {
		cands = s.rerank(ctx, req.Query, cands)
	}

	// 6. Defensive RBAC re-check (defense in depth): drop any candidate whose
	// Dense payload visible_to (if known) does not intersect the user subjects.
	cands = recheckRBAC(cands, dense, scope.SubjectIDs)

	// 7. Assemble, cap to TopN.
	if len(cands) > topN {
		cands = cands[:topN]
	}
	hits := make([]SearchHit, 0, len(cands))
	for _, c := range cands {
		hits = append(hits, SearchHit{
			DocumentID:  c.DocumentID,
			Title:       c.Title,
			ChunkText:   c.ChunkText,
			ChunkIndex:  c.ChunkIndex,
			SectionPath: c.SectionPath,
			Score:       c.FusedScore,
			DenseScore:  c.DenseScore,
			BM25Score:   c.BM25Score,
			RerankScore: c.RerankScore,
			WorkspaceID: c.WorkspaceID,
			SourceURL:   sourceURL(c.WorkspaceID, c.DocumentID),
		})
	}
	return &SearchResult{Items: hits, Total: len(hits)}, nil
}

// rerank re-scores candidates with a Cross-Encoder; on any failure it degrades
// gracefully to the existing fused-score ordering (05 §6.5).
func (s *HybridSearcher) rerank(ctx context.Context, query string, cands []candidate) []candidate {
	rr, err := s.Factory.Reranker(ctx)
	if err != nil || rr == nil {
		return cands
	}
	docs := make([]string, len(cands))
	for i, c := range cands {
		docs[i] = c.ChunkText
	}
	scored, err := rr.Rerank(ctx, query, docs)
	if err != nil {
		s.Logf("rag/search: reranker failed: %v (degrading to fused score)", err)
		return cands
	}
	// scored is desc by score; map back onto candidates.
	for i := range cands {
		cands[i].RerankScore = 0
	}
	for _, sd := range scored {
		if sd.Index >= 0 && sd.Index < len(cands) {
			cands[sd.Index].RerankScore = sd.Score
		}
	}
	// re-sort by rerank score (fall back to fused where rerank is 0).
	sortCandsByRerank(cands)
	return cands
}

func sortCandsByRerank(c []candidate) {
	for i := 1; i < len(c); i++ {
		j := i
		for j > 0 && rerankKey(c[j]) > rerankKey(c[j-1]) {
			c[j], c[j-1] = c[j-1], c[j]
			j--
		}
	}
}
func rerankKey(c candidate) float32 {
	if c.RerankScore != 0 {
		return c.RerankScore
	}
	return c.FusedScore
}

// recheckRBAC is the defense-in-depth filter: any candidate that appeared in the
// Dense results with a visible_to that does NOT intersect the user's subjects is
// dropped. (Dense already filtered server-side; this guards against bugs/leaks.)
func recheckRBAC(cands []candidate, dense []rag.VectorHit, subjects []string) []candidate {
	if len(dense) == 0 {
		return cands // nothing to cross-check (BM25 path already SQL-filtered)
	}
	vis := make(map[string][]string, len(dense))
	for _, h := range dense {
		key := h.Payload.DocumentID + "|" + itoa(h.Payload.ChunkIndex)
		vis[key] = h.Payload.VisibleTo
	}
	subjSet := make(map[string]struct{}, len(subjects))
	for _, s := range subjects {
		subjSet[s] = struct{}{}
	}
	out := cands[:0]
	for _, c := range cands {
		v, ok := vis[c.DocumentID+"|"+itoa(c.ChunkIndex)]
		if !ok {
			// candidate came from BM25 only; trust the SQL-layer RBAC filter.
			out = append(out, c)
			continue
		}
		if intersects(v, subjSet) {
			out = append(out, c)
		}
	}
	return out
}

func intersects(visibleTo []string, subjects map[string]struct{}) bool {
	for _, s := range visibleTo {
		if _, ok := subjects[s]; ok {
			return true
		}
	}
	return false
}

func uniqueDocIDs(cands []candidate) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, c := range cands {
		if _, ok := seen[c.DocumentID]; !ok {
			seen[c.DocumentID] = struct{}{}
			out = append(out, c.DocumentID)
		}
	}
	return out
}

func sourceURL(ws, doc string) string {
	if ws == "" || doc == "" {
		return ""
	}
	return fmt.Sprintf("/workspaces/%s/documents/%s", ws, doc)
}
