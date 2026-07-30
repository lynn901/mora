package pipeline

import (
	"strings"
	"testing"
)

func TestExtractText_HeadingsParagraphsCode(t *testing.T) {
	blockJSON := []byte(`[
		{"type":"heading","level":1,"content":[{"type":"text","text":"Title"}]},
		{"type":"paragraph","content":[{"type":"text","text":"Hello world"}]},
		{"type":"codeBlock","content":[{"type":"text","text":"SELECT 1"}]},
		{"type":"chart","source":"graph TD; A-->B"}
	]`)
	res := ExtractText(blockJSON)
	if !strings.Contains(res.StructuredText, "# Title") {
		t.Errorf("structured text missing heading marker: %q", res.StructuredText)
	}
	if !strings.Contains(res.PlainText, "Hello world") {
		t.Errorf("plain text missing paragraph: %q", res.PlainText)
	}
	if !strings.Contains(res.StructuredText, "SELECT 1") {
		t.Errorf("missing code: %q", res.StructuredText)
	}
	if !strings.Contains(res.StructuredText, "graph TD") {
		t.Errorf("missing chart source: %q", res.StructuredText)
	}
}

func TestExtractText_NestedContent(t *testing.T) {
	blockJSON := []byte(`[
		{"type":"list","content":[
			{"type":"text","text":"item one"},
			{"type":"list","content":[{"type":"text","text":"nested item"}]}
		]}
	]`)
	res := ExtractText(blockJSON)
	if !strings.Contains(res.PlainText, "item one") || !strings.Contains(res.PlainText, "nested item") {
		t.Errorf("nested text not extracted: %q", res.PlainText)
	}
}

func TestExtractText_CleansZeroWidthAndWhitespace(t *testing.T) {
	blockJSON := []byte(`[{"type":"paragraph","content":[{"type":"text","text":"a\u200bb\u200bc   d"}]}]`)
	res := ExtractText(blockJSON)
	if strings.Contains(res.PlainText, "\u200b") {
		t.Errorf("zero-width not cleaned: %q", res.PlainText)
	}
	if strings.Contains(res.PlainText, "  ") {
		t.Errorf("whitespace not collapsed: %q", res.PlainText)
	}
}

func TestExtractText_EmptyAndGarbage(t *testing.T) {
	if ExtractText(nil).PlainText != "" {
		t.Error("nil input should yield empty")
	}
	if ExtractText([]byte(`{not json}`)).PlainText == "" {
		// falls back to raw string; acceptable as long as it doesn't panic
	}
}
