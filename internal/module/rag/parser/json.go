package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// JSONParser parses JSON uploads: a structured value is rendered as a pretty
// code block (preserving keys for FTS). Arrays of objects are also flattened to
// one paragraph per object so each row is independently chunked.
type JSONParser struct{}

func (JSONParser) Name() string { return "json" }

func (JSONParser) Supports(mime, filename string) bool {
	if mime == "application/json" {
		return true
	}
	fn := strings.ToLower(filename)
	return strings.HasSuffix(fn, ".json")
}

func (p JSONParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	b := newBlockBuilder()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Not valid JSON: keep the raw text so the upload is still searchable.
		b.paragraph(string(raw))
		return b.result("json", p.Name(), []string{"invalid JSON, indexed as raw text"})
	}
	switch t := v.(type) {
	case []any:
		// array of objects → one paragraph per element (table-like)
		for i, el := range t {
			pretty, _ := json.MarshalIndent(el, "", "  ")
			b.paragraph(fmt.Sprintf("[%d] %s", i, string(pretty)))
		}
	case map[string]any:
		// object → keys as headings, values as paragraphs (shallow)
		for k, val := range t {
			b.heading(3, k)
			b.paragraph(fmt.Sprintf("%v", val))
		}
	default:
		b.paragraph(fmt.Sprintf("%v", t))
	}
	return b.result("json", p.Name(), nil)
}
