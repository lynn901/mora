package parser

import (
	"context"
	"encoding/json"
	"strings"

	moracontent "github.com/lynn901/mora/internal/module/mora/content"
)

// MarkdownParser converts Markdown uploads to the Block model by reusing the
// existing moracontent.MarkdownToBlocks converter (design §1.3: "MVP 可先复用
// 现有 converter"). The blocks are the same schema documents.content uses, so
// the rest of the pipeline is unchanged. content_text is the raw markdown text
// (the converter's PlainText() would strip structure; we keep markdown so the
// chunker sees heading markers for section boundaries).
type MarkdownParser struct{}

func (MarkdownParser) Name() string { return "markdown" }

func (MarkdownParser) Supports(mime, filename string) bool {
	if mime == "text/markdown" || mime == "text/x-markdown" {
		return true
	}
	fn := strings.ToLower(filename)
	return strings.HasSuffix(fn, ".md") || strings.HasSuffix(fn, ".markdown")
}

func (p MarkdownParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	md := string(raw)
	blocks := moracontent.MarkdownToBlocks(md)
	blocksJSON, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	// content_text: keep the markdown source (headings included) so the
	// pipeline's ExtractText produces structured text with section markers.
	contentText := strings.TrimSpace(md)
	return &ParsedDocument{
		Blocks:      blocksJSON,
		ContentText: contentText,
		Title:       firstHeading(md),
		Meta: ParsedMeta{
			Format:     "md",
			ParserName: p.Name(),
		},
	}, nil
}

// firstHeading returns the text of the first top-level heading, if any.
func firstHeading(md string) string {
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(t[2:])
		}
	}
	return ""
}
