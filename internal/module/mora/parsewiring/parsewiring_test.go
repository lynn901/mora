package parsewiring

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lynn901/mora/internal/module/rag/parser"
)

// TestChunkPreviewerImpl_RespectsParseOptionsChunkSize is the wiring-level
// regression for YS-82: the chunk-preview API (POST /rag/chunk-preview) backed
// by ChunkPreviewerImpl must honor parse_options.chunk_size / chunk_overlap.
// Before the fix, a 500-word terminator-less input collapsed to a single
// chunk regardless of chunk_size, because the underlying chunker only
// hard-split a sentence once it exceeded MaxChunkSize (1024), not chunk_size.
func TestChunkPreviewerImpl_RespectsParseOptionsChunkSize(t *testing.T) {
	words := make([]string, 500)
	for i := range words {
		words[i] = fmt.Sprintf("word%d", i)
	}
	text := strings.Join(words, " ")

	p := ChunkPreviewerImpl{}
	out, err := p.Preview(context.Background(), text, parser.ParseOptions{
		ChunkSize:    64,
		ChunkOverlap: 8,
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if out.Total <= 1 {
		t.Fatalf("expected total > 1 for 500-word text with chunk_size=64, got total=%d", out.Total)
	}
	// each chunk must stay within chunk_size (+1 tokenizer slack for a trailing space).
	for _, c := range out.Chunks {
		if c.TokenCount > 64+1 {
			t.Errorf("chunk %d token_count %d exceeds chunk_size 64+1", c.ChunkIndex, c.TokenCount)
		}
	}
	// contrast: the default (no override) keeps a single oversized chunk for
	// this input only when chunk_size is large enough to hold it; assert the
	// override actually changed the outcome vs. a chunk_size that fits.
	bigOut, err := p.Preview(context.Background(), text, parser.ParseOptions{})
	if err != nil {
		t.Fatalf("preview (default): %v", err)
	}
	if bigOut.Total == out.Total {
		t.Errorf("custom chunk_size produced same total (%d) as default — override not applied", out.Total)
	}
}

// TestChunkPreviewerImpl_DefaultsWhenNoOptions covers the documented default
// path (no parse_options): the previewer falls back to 512/64 and still works.
func TestChunkPreviewerImpl_DefaultsWhenNoOptions(t *testing.T) {
	text := strings.Repeat("word ", 600) // 600 tokens > default chunk_size 512
	p := ChunkPreviewerImpl{}
	out, err := p.Preview(context.Background(), text, parser.ParseOptions{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if out.Total < 2 {
		t.Fatalf("expected >=2 chunks for 600-token text at default chunk_size=512, got %d", out.Total)
	}
	if out.Strategy != "fixed" {
		t.Errorf("strategy = %q want fixed", out.Strategy)
	}
}
