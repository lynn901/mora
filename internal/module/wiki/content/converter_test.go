package content

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wiki/wiki-backend/internal/domain"
)

func TestBlocksToMarkdown_Heading(t *testing.T) {
	blocks := []domain.Block{
		{Type: domain.BlockHeading, Attrs: map[string]any{"level": float64(2)},
			Content: []domain.Block{{Type: domain.BlockText, Text: "Title"}}},
		{Type: domain.BlockParagraph,
			Content: []domain.Block{{Type: domain.BlockText, Text: "Hello world"}}},
	}
	md := BlocksToMarkdown(blocks)
	assert.Equal(t, "## Title\n\nHello world", md)
}

func TestBlocksToMarkdown_CodeBlock(t *testing.T) {
	blocks := []domain.Block{
		{Type: domain.BlockCode, Attrs: map[string]any{"language": "go"}, Text: "fmt.Println(\"hi\")"},
	}
	md := BlocksToMarkdown(blocks)
	assert.Contains(t, md, "```go")
	assert.Contains(t, md, "fmt.Println(\"hi\")")
	assert.Contains(t, md, "```")
}

func TestBlocksToMarkdown_InlineMarks(t *testing.T) {
	blocks := []domain.Block{
		{Type: domain.BlockParagraph, Content: []domain.Block{
			{Type: domain.BlockText, Text: "a "},
			{Type: domain.BlockText, Text: "bold", Marks: []domain.Mark{{Type: "bold"}}},
			{Type: domain.BlockText, Text: " "},
			{Type: domain.BlockText, Text: "ital", Marks: []domain.Mark{{Type: "italic"}}},
			{Type: domain.BlockText, Text: " "},
			{Type: domain.BlockText, Text: "c", Marks: []domain.Mark{{Type: "code"}}},
		}},
	}
	md := BlocksToMarkdown(blocks)
	assert.Equal(t, "a **bold** *ital* `c`", md)
}

func TestMarkdownToBlocks_Heading(t *testing.T) {
	blocks := MarkdownToBlocks("## Title\n\nHello world")
	require.Len(t, blocks, 2)
	assert.Equal(t, domain.BlockHeading, blocks[0].Type)
	assert.Equal(t, float64(2), blocks[0].Attrs["level"])
	assert.Equal(t, "Title", blocks[0].Content[0].Text)
	assert.Equal(t, domain.BlockParagraph, blocks[1].Type)
}

func TestMarkdownToBlocks_CodeBlock(t *testing.T) {
	md := "```go\nfmt.Println(\"hi\")\n```\n\nafter"
	blocks := MarkdownToBlocks(md)
	require.Len(t, blocks, 2)
	assert.Equal(t, domain.BlockCode, blocks[0].Type)
	assert.Equal(t, "go", blocks[0].Attrs["language"])
	assert.Equal(t, "fmt.Println(\"hi\")", blocks[0].Text)
	assert.Equal(t, domain.BlockParagraph, blocks[1].Type)
}

func TestRoundTrip_HeadingParagraph(t *testing.T) {
	orig := []domain.Block{
		{Type: domain.BlockHeading, Attrs: map[string]any{"level": float64(1)},
			Content: []domain.Block{{Type: domain.BlockText, Text: "Doc"}}},
		{Type: domain.BlockParagraph,
			Content: []domain.Block{{Type: domain.BlockText, Text: "Body text here"}}},
	}
	rt := RoundTrip(orig)
	require.Len(t, rt, 2)
	assert.Equal(t, domain.BlockHeading, rt[0].Type)
	assert.Equal(t, float64(1), rt[0].Attrs["level"])
	assert.Equal(t, "Doc", rt[0].Content[0].Text)
	assert.Equal(t, domain.BlockParagraph, rt[1].Type)
	assert.Equal(t, "Body text here", rt[1].Content[0].Text)
}

func TestRoundTrip_CodeBlock(t *testing.T) {
	orig := []domain.Block{
		{Type: domain.BlockCode, Attrs: map[string]any{"language": "bash"}, Text: "echo hi\necho bye"},
	}
	rt := RoundTrip(orig)
	require.Len(t, rt, 1)
	assert.Equal(t, domain.BlockCode, rt[0].Type)
	assert.Equal(t, "bash", rt[0].Attrs["language"])
	assert.Equal(t, "echo hi\necho bye", rt[0].Text)
}

func TestRoundTrip_List(t *testing.T) {
	orig := []domain.Block{
		{Type: domain.BlockList, Attrs: map[string]any{"order": "bullet"}, Content: []domain.Block{
			{Type: "listItem", Content: []domain.Block{{Type: domain.BlockText, Text: "one"}}},
			{Type: "listItem", Content: []domain.Block{{Type: domain.BlockText, Text: "two"}}},
		}},
	}
	rt := RoundTrip(orig)
	require.Len(t, rt, 1)
	assert.Equal(t, domain.BlockList, rt[0].Type)
	assert.Equal(t, "bullet", rt[0].Attrs["order"])
	require.Len(t, rt[0].Content, 2)
	assert.Equal(t, "one", rt[0].Content[0].Content[0].Text)
	assert.Equal(t, "two", rt[0].Content[1].Content[0].Text)
}

func TestRoundTrip_Blockquote(t *testing.T) {
	orig := []domain.Block{
		{Type: domain.BlockQuote, Content: []domain.Block{{Type: domain.BlockText, Text: "quoted text"}}},
	}
	rt := RoundTrip(orig)
	require.Len(t, rt, 1)
	assert.Equal(t, domain.BlockQuote, rt[0].Type)
	assert.Equal(t, "quoted text", rt[0].Content[0].Text)
}

func TestRoundTrip_Divider(t *testing.T) {
	orig := []domain.Block{
		{Type: domain.BlockParagraph, Content: []domain.Block{{Type: domain.BlockText, Text: "before"}}},
		{Type: domain.BlockDivider},
		{Type: domain.BlockParagraph, Content: []domain.Block{{Type: domain.BlockText, Text: "after"}}},
	}
	rt := RoundTrip(orig)
	require.Len(t, rt, 3)
	assert.Equal(t, domain.BlockDivider, rt[1].Type)
}

func TestParseInline_BoldItalicCode(t *testing.T) {
	// "a **b** *c* `d`" → a , bold b, " ", italic c, " ", code d
	nodes := parseInline("a **b** *c* `d`")
	require.Len(t, nodes, 6, "nodes: %+v", nodes)
	assert.Equal(t, "a ", nodes[0].Text)
	assert.Equal(t, "b", nodes[1].Text)
	assert.True(t, hasMark(&nodes[1], "bold"))
	assert.Equal(t, " ", nodes[2].Text)
	assert.Equal(t, "c", nodes[3].Text)
	assert.True(t, hasMark(&nodes[3], "italic"))
	assert.Equal(t, " ", nodes[4].Text)
	assert.Equal(t, "d", nodes[5].Text)
	assert.True(t, hasMark(&nodes[5], "code"))
}

func TestPlainText(t *testing.T) {
	blocks := domain.BlockArray{
		{Type: domain.BlockHeading, Content: []domain.Block{{Type: domain.BlockText, Text: "Title"}}},
		{Type: domain.BlockParagraph, Content: []domain.Block{
			{Type: domain.BlockText, Text: "Hello"},
			{Type: domain.BlockText, Text: "world"},
		}},
	}
	pt := blocks.PlainText()
	assert.Contains(t, pt, "Title")
	assert.Contains(t, pt, "Hello")
	assert.Contains(t, pt, "world")
}
