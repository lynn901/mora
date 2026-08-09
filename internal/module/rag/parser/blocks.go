package parser

import (
	"encoding/json"
	"strings"

	"github.com/lynn901/mora/internal/domain"
)

// blockBuilder assembles a []domain.Block and the plain-text companion. Every
// parser builds content through it so the output stays schema-compatible with
// documents.content (Block JSONB) and the pipeline ExtractText() walker.
type blockBuilder struct {
	blocks    []domain.Block
	textParts []string
}

func newBlockBuilder() *blockBuilder { return &blockBuilder{} }

func (b *blockBuilder) heading(level int, text string) *blockBuilder {
	if strings.TrimSpace(text) == "" {
		return b
	}
	b.blocks = append(b.blocks, domain.Block{
		Type:  domain.BlockHeading,
		Attrs: map[string]any{"level": level},
		Content: []domain.Block{
			{Type: "text", Text: text},
		},
	})
	b.textParts = append(b.textParts, text)
	return b
}

func (b *blockBuilder) paragraph(text string) *blockBuilder {
	text = strings.TrimSpace(text)
	if text == "" {
		return b
	}
	b.blocks = append(b.blocks, domain.Block{
		Type: domain.BlockParagraph,
		Content: []domain.Block{
			{Type: "text", Text: text},
		},
	})
	b.textParts = append(b.textParts, text)
	return b
}

// paragraphMulti appends a paragraph whose content spans multiple inline text
// nodes (used by list items / table cells when callers want them as one block).
func (b *blockBuilder) paragraphText(parts ...string) *blockBuilder {
	kept := make([]domain.Block, 0, len(parts))
	var tp []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, domain.Block{Type: "text", Text: p})
			tp = append(tp, p)
		}
	}
	if len(kept) == 0 {
		return b
	}
	b.blocks = append(b.blocks, domain.Block{Type: domain.BlockParagraph, Content: kept})
	b.textParts = append(b.textParts, strings.Join(tp, " "))
	return b
}

func (b *blockBuilder) code(language, source string) *blockBuilder {
	source = strings.TrimRight(source, "\n")
	if source == "" {
		return b
	}
	b.blocks = append(b.blocks, domain.Block{
		Type:  domain.BlockCode,
		Attrs: map[string]any{"language": language},
		Text:  source,
	})
	b.textParts = append(b.textParts, source)
	return b
}

func (b *blockBuilder) quote(text string) *blockBuilder {
	text = strings.TrimSpace(text)
	if text == "" {
		return b
	}
	b.blocks = append(b.blocks, domain.Block{
		Type: domain.BlockQuote,
		Content: []domain.Block{
			{Type: "text", Text: text},
		},
	})
	b.textParts = append(b.textParts, text)
	return b
}

func (b *blockBuilder) divider() *blockBuilder {
	b.blocks = append(b.blocks, domain.Block{Type: domain.BlockDivider})
	return b
}

// orderedList / bulletList emit list blocks; items is a flat list of strings.
func (b *blockBuilder) list(ordered bool, items []string) *blockBuilder {
	var content []domain.Block
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		content = append(content, domain.Block{
			Type:    "listItem",
			Content: []domain.Block{{Type: "text", Text: it}},
		})
		b.textParts = append(b.textParts, it)
	}
	if len(content) == 0 {
		return b
	}
	order := "unordered"
	if ordered {
		order = "ordered"
	}
	b.blocks = append(b.blocks, domain.Block{
		Type:    domain.BlockList,
		Attrs:   map[string]any{"order": order},
		Content: content,
	})
	return b
}

// result marshals the blocks to JSONB and joins the plain text.
func (b *blockBuilder) result(format, parserName string, warnings []string) (*ParsedDocument, error) {
	return b.resultWithMeta(ParsedMeta{Format: format, ParserName: parserName, Warnings: warnings})
}

// resultWithMeta marshals the blocks with caller-supplied meta (page count,
// custom warnings). Used by parsers that track pages (PDF).
func (b *blockBuilder) resultWithMeta(meta ParsedMeta) (*ParsedDocument, error) {
	blocksJSON, err := json.Marshal(b.blocks)
	if err != nil {
		return nil, err
	}
	return &ParsedDocument{
		Blocks:      blocksJSON,
		ContentText: strings.Join(b.textParts, "\n"),
		Meta:        meta,
	}, nil
}

// isEmpty reports whether no blocks were appended.
func (b *blockBuilder) isEmpty() bool { return len(b.blocks) == 0 }
