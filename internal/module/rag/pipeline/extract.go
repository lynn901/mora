package pipeline

import (
	"encoding/json"
	"strings"
	"unicode"
)

// ExtractResult is the output of text extraction from a document's Block JSONB.
// StructuredText keeps Markdown heading markers (`#`/`##`/`###`) so the chunker
// can respect section boundaries; PlainText is the cleaned text used for FTS.
type ExtractResult struct {
	StructuredText string
	PlainText      string
}

// ExtractText walks a document's Block JSONB array and extracts indexable text.
// It is intentionally tolerant of the exact block schema: any object carrying a
// "text" leaf contributes; heading blocks (`type` contains "heading", with an
// optional `level`) emit Markdown `#` markers to mark section boundaries.
//
// Block types handled (per 05 §3.2): heading/paragraph → text; codeBlock → code
// (marked `code:`); chart/canvas → mermaid source / description; decorative
// blocks are skipped. Attachments are extracted upstream and
// passed in via DocumentSnapshot.ContentText; this function only parses blocks.
func ExtractText(blockJSON []byte) ExtractResult {
	if len(blockJSON) == 0 {
		return ExtractResult{}
	}
	var blocks []map[string]any
	if err := json.Unmarshal(blockJSON, &blocks); err != nil {
		// Not an array: maybe a single block object or plain text. Fall back.
		if s := string(blockJSON); strings.TrimSpace(s) != "" {
			return ExtractResult{StructuredText: s, PlainText: clean(s)}
		}
		return ExtractResult{}
	}
	var structB, plainB strings.Builder
	for _, blk := range blocks {
		writeBlock(&structB, &plainB, blk, 1)
	}
	return ExtractResult{
		StructuredText: strings.TrimSpace(removeZeroWidth(structB.String())),
		PlainText:      clean(plainB.String()),
	}
}

func writeBlock(structB, plainB *strings.Builder, blk map[string]any, level int) {
	btype, _ := blk["type"].(string)
	switch {
	case strings.Contains(strings.ToLower(btype), "heading"):
		lvl := levelFromBlock(blk, level)
		text := collectText(blk)
		if text == "" {
			return
		}
		marks := strings.Repeat("#", clampLevel(lvl))
		structB.WriteString("\n" + marks + " " + text + "\n")
		plainB.WriteString(text + "\n")
	case btype == "codeBlock" || btype == "code":
		code := collectText(blk)
		if code == "" {
			return
		}
		structB.WriteString("\ncode:\n" + code + "\n")
		plainB.WriteString(code + "\n")
	case btype == "chart" || btype == "canvas":
		// Prefer mermaid source; fall back to description.
		text := firstString(blk, "source", "mermaid", "description", "text")
		if text == "" {
			text = collectText(blk)
		}
		if text == "" {
			return
		}
		structB.WriteString("\n" + text + "\n")
		plainB.WriteString(text + "\n")
	default:
		// paragraph, list, quote, table cell, etc. collectText already recurses
		// through content/children and gathers every text leaf, so we must NOT
		// recurse again here (that would double-emit nested text).
		text := collectText(blk)
		if text != "" {
			structB.WriteString(text + "\n")
			plainB.WriteString(text + "\n")
		}
	}
}

// collectText gathers every "text" leaf anywhere under blk (depth-first).
func collectText(blk map[string]any) string {
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if s, ok := t["text"].(string); ok && s != "" {
				b.WriteString(s)
				b.WriteByte(' ')
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(blk)
	return strings.TrimSpace(b.String())
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func levelFromBlock(blk map[string]any, def int) int {
	if l, ok := blk["level"].(float64); ok && l > 0 {
		return int(l)
	}
	if a, ok := blk["attrs"].(map[string]any); ok {
		if l, ok := a["level"].(float64); ok && l > 0 {
			return int(l)
		}
	}
	return def
}

func clampLevel(l int) int {
	if l < 1 {
		return 1
	}
	if l > 6 {
		return 6
	}
	return l
}

// clean removes zero-width characters and collapses whitespace (05 §3.2).
func clean(s string) string {
	s = removeZeroWidth(s)
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

// removeZeroWidth strips zero-width characters but preserves other whitespace
// (used on structured text so heading markers/newlines survive).
func removeZeroWidth(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case 0x200B, 0x200C, 0x200D, 0xFEFF:
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
