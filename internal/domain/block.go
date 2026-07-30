package domain

import "encoding/json"

// BlockType enumerates the supported block kinds (PRD F1.1 / API schema Block.type).
type BlockType string

const (
	BlockHeading   BlockType = "heading"
	BlockParagraph BlockType = "paragraph"
	BlockText      BlockType = "text"
	BlockCode      BlockType = "codeBlock"
	BlockQuote     BlockType = "blockquote"
	BlockList      BlockType = "list"
	BlockDivider   BlockType = "divider"
	BlockChart     BlockType = "chart"
	BlockCanvas    BlockType = "canvas"
)

// Block is the unit of structured content stored in documents.content (JSONB).
// It follows the TipTap/ProseMirror node shape: {type, attrs, content[]}.
type Block struct {
	ID      string          `json:"id,omitempty"`
	Type    BlockType       `json:"type"`
	Attrs   map[string]any  `json:"attrs,omitempty"`
	Content []Block         `json:"content,omitempty"`
	Text    string          `json:"text,omitempty"`
	Marks   []Mark          `json:"marks,omitempty"`
}

type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// MarshalJSON renders a Block with its text inline when present (TipTap leaf text node).
func (b Block) MarshalJSON() ([]byte, error) {
	type alias Block
	return json.Marshal((alias)(b))
}

// BlockArray is a helper for JSONB round-tripping.
type BlockArray []Block

// PlainText extracts concatenated visible text from a block tree (for FTS indexing).
func (bs BlockArray) PlainText() string {
	var buf []byte
	for i := range bs {
		bs[i].appendText(&buf)
	}
	return string(buf)
}

func (b *Block) appendText(buf *[]byte) {
	if b.Text != "" {
		*buf = append(*buf, ' ')
		*buf = append(*buf, b.Text...)
	}
	for i := range b.Content {
		b.Content[i].appendText(buf)
	}
}
