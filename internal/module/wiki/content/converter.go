package content

// Package content implements bidirectional conversion between Markdown and the
// Block (TipTap/ProseMirror-like) JSON model (PRD F1.1, AC-1: Markdown 与富
// 文本内容存储双向可逆转换，不丢失语义).
//
// Supported block types: heading(1-6), paragraph, codeBlock, blockquote,
// bulletList, orderedList, divider. Inline marks: bold(**), italic(*),
// code(`). The round-trip Blocks→Markdown→Blocks preserves structure for the
// supported subset; unsupported constructs degrade gracefully.

import (
	"strings"

	"github.com/wiki/wiki-backend/internal/domain"
)

// BlocksToMarkdown renders a block array as Markdown text.
func BlocksToMarkdown(blocks []domain.Block) string {
	var b strings.Builder
	for i := range blocks {
		writeBlockMarkdown(&b, &blocks[i])
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeBlockMarkdown(b *strings.Builder, blk *domain.Block) {
	switch blk.Type {
	case domain.BlockHeading:
		lvl := 1
		if v, ok := blk.Attrs["level"].(float64); ok {
			lvl = int(v)
		}
		if lvl < 1 {
			lvl = 1
		}
		if lvl > 6 {
			lvl = 6
		}
		b.WriteString(strings.Repeat("#", lvl))
		b.WriteByte(' ')
		b.WriteString(inlineToMarkdown(blk.Content))
		b.WriteString("\n\n")
	case domain.BlockParagraph:
		b.WriteString(inlineToMarkdown(blk.Content))
		b.WriteString("\n\n")
	case domain.BlockCode:
		lang := ""
		if v, ok := blk.Attrs["language"].(string); ok {
			lang = v
		}
		b.WriteString("```")
		b.WriteString(lang)
		b.WriteString("\n")
		b.WriteString(extractText(blk))
		b.WriteString("\n```\n\n")
	case domain.BlockQuote:
		text := inlineToMarkdown(blk.Content)
		for _, line := range strings.Split(text, "\n") {
			b.WriteString("> ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	case domain.BlockList:
		ordered := false
		if v, ok := blk.Attrs["order"].(string); ok && v == "ordered" {
			ordered = true
		}
		for i, item := range blk.Content {
			prefix := "- "
			if ordered {
				prefix = itoa(i+1) + ". "
			}
			b.WriteString(prefix)
			b.WriteString(inlineToMarkdown(item.Content))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	case domain.BlockDivider:
		b.WriteString("---\n\n")
	default:
		b.WriteString(inlineToMarkdown(blk.Content))
		b.WriteString("\n\n")
	}
}

// MarkdownToBlocks parses Markdown into a block array.
func MarkdownToBlocks(md string) []domain.Block {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var blocks []domain.Block
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			i++
			continue
		}

		// Fenced code block
		if strings.HasPrefix(trimmed, "```") {
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			var code []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++ // skip closing fence
			blocks = append(blocks, domain.Block{
				Type:  domain.BlockCode,
				Attrs: map[string]any{"language": lang},
				Text:  strings.Join(code, "\n"),
			})
			continue
		}

		// Divider
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			blocks = append(blocks, domain.Block{Type: domain.BlockDivider})
			i++
			continue
		}

		// Heading
		if h := headingLevel(trimmed); h > 0 {
			blocks = append(blocks, domain.Block{
				Type:  domain.BlockHeading,
				Attrs: map[string]any{"level": float64(h)},
				Content: parseInline(strings.TrimSpace(trimmed[h:])),
			})
			i++
			continue
		}

		// Blockquote
		if strings.HasPrefix(trimmed, "> ") {
			var quote []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "> ") {
				quote = append(quote, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "> ")))
				i++
			}
			blocks = append(blocks, domain.Block{
				Type:    domain.BlockQuote,
				Content: parseInline(strings.Join(quote, "\n")),
			})
			continue
		}

		// List (consecutive - or N. items)
		if isListItem(trimmed) {
			ordered := strings.Contains(trimmed, ".") && !strings.HasPrefix(trimmed, "-")
			var items []domain.Block
			for i < len(lines) && isListItem(strings.TrimSpace(lines[i])) {
				item := strings.TrimSpace(lines[i])
				if ordered {
					if dot := strings.Index(item, "."); dot >= 0 {
						item = strings.TrimSpace(item[dot+1:])
					}
				} else if strings.HasPrefix(item, "- ") {
					item = item[2:]
				}
				items = append(items, domain.Block{Type: "listItem", Content: parseInline(item)})
				i++
			}
			attrs := map[string]any{"order": "bullet"}
			if ordered {
				attrs["order"] = "ordered"
			}
			blocks = append(blocks, domain.Block{Type: domain.BlockList, Attrs: attrs, Content: items})
			continue
		}

		// Paragraph (consume consecutive non-empty, non-special lines)
		var para []string
		for i < len(lines) {
			l := strings.TrimSpace(lines[i])
			if l == "" || isSpecial(l) {
				break
			}
			para = append(para, l)
			i++
		}
		blocks = append(blocks, domain.Block{
			Type:    domain.BlockParagraph,
			Content: parseInline(strings.Join(para, " ")),
		})
	}
	return blocks
}

// RoundTrip converts blocks→markdown→blocks; used to verify reversibility.
func RoundTrip(blocks []domain.Block) []domain.Block {
	return MarkdownToBlocks(BlocksToMarkdown(blocks))
}

// --- inline handling ---

func inlineToMarkdown(nodes []domain.Block) string {
	var b strings.Builder
	for i := range nodes {
		writeInline(&b, &nodes[i])
	}
	return b.String()
}

func writeInline(b *strings.Builder, n *domain.Block) {
	if n.Text != "" {
		text := n.Text
		bold := hasMark(n, "bold")
		italic := hasMark(n, "italic")
		code := hasMark(n, "code")
		if code {
			b.WriteString("`")
			b.WriteString(text)
			b.WriteString("`")
			return
		}
		if bold {
			b.WriteString("**")
		}
		if italic {
			b.WriteString("*")
		}
		b.WriteString(text)
		if italic {
			b.WriteString("*")
		}
		if bold {
			b.WriteString("**")
		}
		return
	}
	for i := range n.Content {
		writeInline(b, &n.Content[i])
	}
}

// parseInline splits a string into text nodes with marks for **bold**, *italic*, `code`.
// It scans left-to-right, always taking the earliest marker; on a tie between
// "**" and "*" at the same position, bold wins (longer match).
func parseInline(s string) []domain.Block {
	var nodes []domain.Block
	pos := 0
	for pos < len(s) {
		codeIdx := indexByte(s, '`', pos)
		boldIdx := indexStr(s, "**", pos)
		italIdx := indexItalic(s, pos)

		earliest, marker := pickEarliest(codeIdx, "code", boldIdx, "bold", italIdx, "italic")
		if earliest < 0 {
			nodes = appendText(nodes, s[pos:])
			break
		}

		switch marker {
		case "code":
			rest := s[earliest+1:]
			end := indexByte(rest, '`', 0)
			if end < 0 {
				nodes = appendText(nodes, s[pos:])
				pos = len(s)
				continue
			}
			if earliest > pos {
				nodes = appendText(nodes, s[pos:earliest])
			}
			nodes = append(nodes, domain.Block{Type: domain.BlockText, Text: rest[:end], Marks: []domain.Mark{{Type: "code"}}})
			pos = earliest + 1 + end + 1
		case "bold":
			rest := s[earliest+2:]
			end := indexStr(rest, "**", 0)
			if end < 0 {
				nodes = appendText(nodes, s[pos:])
				pos = len(s)
				continue
			}
			if earliest > pos {
				nodes = appendText(nodes, s[pos:earliest])
			}
			nodes = append(nodes, domain.Block{Type: domain.BlockText, Text: rest[:end], Marks: []domain.Mark{{Type: "bold"}}})
			pos = earliest + 2 + end + 2
		case "italic":
			rest := s[earliest+1:]
			end := indexItalic(rest, 0)
			if end < 0 {
				nodes = appendText(nodes, s[pos:])
				pos = len(s)
				continue
			}
			if earliest > pos {
				nodes = appendText(nodes, s[pos:earliest])
			}
			nodes = append(nodes, domain.Block{Type: domain.BlockText, Text: rest[:end], Marks: []domain.Mark{{Type: "italic"}}})
			pos = earliest + 1 + end + 1
		}
	}
	if len(nodes) == 0 {
		nodes = appendText(nodes, "")
	}
	return nodes
}

// pickEarliest returns the smallest non-negative index among codeIdx/boldIdx/italIdx
// and the marker name. Ties at the same position favor bold over italic (longer
// marker wins), and code is independent. Returns (-1,"") if all are -1.
func pickEarliest(codeIdx int, codeMark string, boldIdx int, boldMark string, italIdx int, italMark string) (int, string) {
	type cand struct {
		idx   int
		mark  string
		order int
	}
	cands := []cand{
		{codeIdx, codeMark, 2},
		{boldIdx, boldMark, 0},
		{italIdx, italMark, 1},
	}
	best := -1
	var bestMark string
	bestOrder := 99
	for _, c := range cands {
		if c.idx < 0 {
			continue
		}
		if best < 0 || c.idx < best || (c.idx == best && c.order < bestOrder) {
			best = c.idx
			bestMark = c.mark
			bestOrder = c.order
		}
	}
	return best, bestMark
}

func appendText(nodes []domain.Block, s string) []domain.Block {
	return append(nodes, domain.Block{Type: domain.BlockText, Text: s})
}

// indexByte returns the index of byte c in s[from:], or -1.
func indexByte(s string, c byte, from int) int {
	if from >= len(s) {
		return -1
	}
	if i := strings.IndexByte(s[from:], c); i >= 0 {
		return from + i
	}
	return -1
}

// indexStr returns the index of substr in s[from:], or -1.
func indexStr(s, substr string, from int) int {
	if from >= len(s) {
		return -1
	}
	if i := strings.Index(s[from:], substr); i >= 0 {
		return from + i
	}
	return -1
}

// indexItalic finds a single '*' not part of a '**' pair, from position `from`.
func indexItalic(s string, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] != '*' {
			continue
		}
		prev := i == 0 || s[i-1] != '*'
		next := i+1 >= len(s) || s[i+1] != '*'
		if prev && next {
			return i
		}
	}
	return -1
}

func hasMark(n *domain.Block, t string) bool {
	for _, m := range n.Marks {
		if m.Type == t {
			return true
		}
	}
	return false
}

func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	if n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n
}

func isListItem(s string) bool {
	if strings.HasPrefix(s, "- ") {
		return true
	}
	// ordered: "1. " etc
	dot := strings.Index(s, ". ")
	if dot <= 0 {
		return false
	}
	num := s[:dot]
	for _, c := range num {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isSpecial(s string) bool {
	return headingLevel(s) > 0 || strings.HasPrefix(s, "> ") ||
		strings.HasPrefix(s, "```") || s == "---" || s == "***" || s == "___" ||
		isListItem(s)
}

func extractText(blk *domain.Block) string {
	if blk.Text != "" {
		return blk.Text
	}
	var b strings.Builder
	for i := range blk.Content {
		t := extractText(&blk.Content[i])
		b.WriteString(t)
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
