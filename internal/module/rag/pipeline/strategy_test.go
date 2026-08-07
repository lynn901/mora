package pipeline

import (
	"strings"
	"testing"
)

func TestChunk_Adaptive3Tier_MergesSmallSections(t *testing.T) {
	// Three tiny sections (each < chunkSize/2) under separate headings → the
	// adaptive merger should combine them into fewer chunks than the fixed
	// splitter, which would emit one chunk per tiny section.
	var b strings.Builder
	b.WriteString("# S1\n" + strings.Repeat("a ", 5) + "\n\n")
	b.WriteString("# S2\n" + strings.Repeat("b ", 5) + "\n\n")
	b.WriteString("# S3\n" + strings.Repeat("c ", 5) + "\n\n")
	text := b.String()
	tok := NewWordTokenizer()

	cfgFixed := Config{ChunkSize: 512, ChunkOverlap: 64, RespectHeadingBoundary: true, Strategy: StrategyFixed}
	cfgAdaptive := Config{ChunkSize: 512, ChunkOverlap: 64, RespectHeadingBoundary: true, Strategy: StrategyAdaptive3Tier}

	fixed := Chunk(text, cfgFixed, tok)
	adaptive := Chunk(text, cfgAdaptive, tok)
	if len(adaptive) >= len(fixed) {
		t.Errorf("adaptive should merge small sections: adaptive=%d fixed=%d", len(adaptive), len(fixed))
	}
	// merged chunk should carry the union of section paths
	if len(adaptive) > 0 {
		joined := strings.Join(sectionPaths(adaptive), " ")
		if !strings.Contains(joined, "S1") || !strings.Contains(joined, "S3") {
			t.Errorf("merged section paths missing sections: %v", sectionPaths(adaptive))
		}
	}
}

func TestChunk_Adaptive3Tier_SplitsLargeSection(t *testing.T) {
	// a single section far larger than chunkSize must still split (Tier 3).
	var b strings.Builder
	b.WriteString("# Big\n")
	b.WriteString(strings.Repeat("word ", 2000))
	text := b.String()
	tok := NewWordTokenizer()
	cfg := Config{ChunkSize: 20, ChunkOverlap: 4, MaxChunkSize: 40, Strategy: StrategyAdaptive3Tier, RespectHeadingBoundary: true}
	chunks := Chunk(text, cfg, tok)
	if len(chunks) < 2 {
		t.Fatalf("expected large section to split, got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if c.TokenCount > cfg.MaxChunkSize+1 {
			t.Errorf("chunk %d tokens %d exceeds max %d", c.ChunkIndex, c.TokenCount, cfg.MaxChunkSize)
		}
	}
}

func TestChunk_ParentChild_EmitsParentAndChildren(t *testing.T) {
	// a section larger than chunkSize → one parent + multiple children, all
	// linked. A small section → one standalone chunk (no parent/child).
	var b strings.Builder
	b.WriteString("# Big Section\n")
	b.WriteString(strings.Repeat("content ", 200))
	b.WriteString("\n\n# Small\nshorty")
	text := b.String()
	tok := NewWordTokenizer()
	cfg := Config{ChunkSize: 20, ChunkOverlap: 4, MaxChunkSize: 40, Strategy: StrategyParentChild, RespectHeadingBoundary: true}
	chunks := Chunk(text, cfg, tok)

	var parents, children, standalones int
	childParentMap := map[int]int{}
	for _, c := range chunks {
		switch c.Role {
		case RoleParent:
			parents++
		case RoleChild:
			children++
			childParentMap[c.ChunkIndex] = c.ParentChunkIndex
		case RoleStandalone:
			standalones++
		}
	}
	if parents == 0 {
		t.Errorf("expected at least one parent chunk for the big section")
	}
	if children == 0 {
		t.Errorf("expected child chunks under the big section parent")
	}
	if standalones == 0 {
		t.Errorf("expected the small section to be a standalone chunk")
	}
	// every child's parent index must point at an existing parent chunk
	parentIdxs := map[int]bool{}
	for _, c := range chunks {
		if c.Role == RoleParent {
			parentIdxs[c.ChunkIndex] = true
		}
	}
	for _, pidx := range childParentMap {
		if !parentIdxs[pidx] {
			t.Errorf("child points at non-existent parent index %d", pidx)
		}
	}
}

func TestChunk_ParentChild_NoHeadingsTreatsWholeAsOneSection(t *testing.T) {
	// no headings → splitSections returns one section (the whole text). A
	// large-enough text becomes one parent + children; a small text becomes
	// one standalone. This is the correct behavior: parent/child is driven by
	// section boundaries, and a heading-less document is one section.
	text := strings.Repeat("plain text content here ", 30)
	tok := NewWordTokenizer()
	cfg := Config{ChunkSize: 20, ChunkOverlap: 4, MaxChunkSize: 40, Strategy: StrategyParentChild, RespectHeadingBoundary: true}
	chunks := Chunk(text, cfg, tok)
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	// roles must be internally consistent: every child has a parent index
	// that exists in the chunk list.
	roles := map[string]bool{}
	parentIdxs := map[int]bool{}
	for _, c := range chunks {
		roles[string(c.Role)] = true
		if c.Role == RoleParent {
			parentIdxs[c.ChunkIndex] = true
		}
	}
	if !roles["parent"] {
		t.Errorf("expected a parent for the single large section")
	}
	for _, c := range chunks {
		if c.Role == RoleChild && !parentIdxs[c.ParentChunkIndex] {
			t.Errorf("child points at non-existent parent %d", c.ParentChunkIndex)
		}
	}
}

func TestChunk_StrategyDefaultIsFixed(t *testing.T) {
	// DefaultConfig().Strategy is fixed → existing behavior unchanged.
	cfg := DefaultConfig()
	if cfg.Strategy != StrategyFixed {
		t.Errorf("default strategy = %q, want fixed", cfg.Strategy)
	}
	// a zero-Strategy config (legacy callers) must also route to fixed.
	text := "# H\nsome text"
	chunks := Chunk(text, Config{ChunkSize: 512, ChunkOverlap: 64}, NewWordTokenizer())
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
}

func sectionPaths(chunks []ChunkRef) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.SectionPath
	}
	return out
}
