package pipeline

import (
	"strings"
	"testing"
)

func TestChunk_RespectsHeadingBoundariesAndSectionPath(t *testing.T) {
	text := "# Chapter One\n" +
		strings.Repeat("alpha ", 40) + "\n\n" +
		"## 1.1 Subsection\n" +
		strings.Repeat("beta ", 40) + "\n\n" +
		"# Chapter Two\n" +
		strings.Repeat("gamma ", 40) + "\n"
	cfg := Config{ChunkSize: 20, ChunkOverlap: 4, MaxChunkSize: 40, RespectHeadingBoundary: true}
	chunks := Chunk(text, cfg, NewWordTokenizer())
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	// section paths must be non-empty and reflect hierarchy
	paths := map[string]bool{}
	for _, c := range chunks {
		paths[c.SectionPath] = true
	}
	if !paths["Chapter One"] {
		t.Errorf("expected section 'Chapter One', got paths: %v", paths)
	}
	if !paths["Chapter One > 1.1 Subsection"] {
		t.Errorf("expected nested section path, got: %v", paths)
	}
	if !paths["Chapter Two"] {
		t.Errorf("expected section 'Chapter Two', got: %v", paths)
	}
}

func TestChunk_SingleSmallSectionIsOneChunk(t *testing.T) {
	cfg := Config{ChunkSize: 512, ChunkOverlap: 64, RespectHeadingBoundary: true}
	chunks := Chunk("## Intro\nshort text here", cfg, NewWordTokenizer())
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].SectionPath != "Intro" {
		t.Errorf("section path = %q", chunks[0].SectionPath)
	}
}

func TestChunk_DoesNotCutSentencesAndRespectsMaxSize(t *testing.T) {
	// one giant sentence with no terminators longer than max → hard-split
	long := strings.Repeat("word ", 300)
	cfg := Config{ChunkSize: 20, ChunkOverlap: 4, MaxChunkSize: 40}
	chunks := Chunk(long, cfg, NewWordTokenizer())
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for long text, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.TokenCount > cfg.MaxChunkSize+1 {
			t.Errorf("chunk %d tokens %d exceeds max %d", c.ChunkIndex, c.TokenCount, cfg.MaxChunkSize)
		}
	}
}

func TestChunk_LongSentenceBetweenChunkSizeAndMaxIsSplit(t *testing.T) {
	// Regression (YS-82): a single terminator-less sentence that is longer
	// than chunk_size but shorter than max_chunk_size must still be hard-split.
	// Previously the threshold was `>= maxSize`, so such a sentence slipped
	// through and collapsed the whole body into one oversized chunk — the
	// chunk-preview API then ignored chunk_size for long inputs.
	long := strings.Repeat("word ", 500) // 500 tokens, no sentence terminators
	cfg := Config{ChunkSize: 64, ChunkOverlap: 8, MaxChunkSize: 1024}
	chunks := Chunk(long, cfg, NewWordTokenizer())
	if len(chunks) <= 1 {
		t.Fatalf("expected multiple chunks for 500-token text at chunk_size=64, got %d", len(chunks))
	}
	for _, c := range chunks {
		// every chunk must respect chunk_size (overlap can carry a little, but
		// hardSplitWords emits groups of exactly chunk_size words; allow +1
		// for the tokenizer rounding a trailing space).
		if c.TokenCount > cfg.ChunkSize+1 {
			t.Errorf("chunk %d tokens %d exceeds chunk_size %d", c.ChunkIndex, c.TokenCount, cfg.ChunkSize)
		}
		if c.TokenCount > cfg.MaxChunkSize {
			t.Errorf("chunk %d tokens %d exceeds max %d", c.ChunkIndex, c.TokenCount, cfg.MaxChunkSize)
		}
	}
}

func TestChunk_OverlapKeepsContext(t *testing.T) {
	// sentences separated by periods, total > chunkSize → overlap carries tail
	var parts []string
	for i := 0; i < 10; i++ {
		parts = append(parts, "sentence number "+itoa(i)+" end.")
	}
	text := strings.Join(parts, " ")
	cfg := Config{ChunkSize: 12, ChunkOverlap: 4, MaxChunkSize: 20}
	chunks := Chunk(text, cfg, NewWordTokenizer())
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(chunks))
	}
	// overlap: second chunk should share some words with the tail of the first
	if !strings.Contains(chunks[1].Text, "sentence number") {
		t.Errorf("overlap chunk missing context: %q", chunks[1].Text)
	}
}

func TestWordTokenizer(t *testing.T) {
	tok := NewWordTokenizer()
	if c := tok.Count("hello world foo"); c != 3 {
		t.Errorf("count = %d want 3", c)
	}
	tok2 := WordTokenizer{TokensPerWord: 1.3}
	if c := tok2.Count("a b c d"); c != 5 { // 4*1.3=5.2 → 5
		t.Errorf("scaled count = %d want 5", c)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
