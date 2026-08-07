package parser

import "strings"

// DefaultRegistry returns a Registry pre-registered with all the pure-Go
// parsers shipped in this package (P0 + P1 formats). P2 multimodal parsers
// (the mora-parser sidecar for OCR/VLM/ASR) are wired separately when their
// sidecar URL is configured — see the sidecar.go file.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	// Registration order: most-specific extensions first so a .md.txt edge
	// case still routes to the markdown parser (it Supports() both, but text
	// is registered last). Lookup walks in registration order.
	for _, p := range []Parser{
		MarkdownParser{},
		HTMLParser{},
		JSONParser{},
		CSVParser{},
		PDFParser{},
		DocxParser{},
		XlsxParser{},
		PptxParser{},
		EpubParser{},
		MhtmlParser{},
		TextParser{}, // most permissive — registered last as the fallback
	} {
		r.Register(p)
	}
	return r
}

// IsPlainText is a small helper used by upload handlers to decide whether a
// format can be parsed inline synchronously (small enough) vs. must enqueue.
func IsPlainText(format string) bool {
	switch strings.ToLower(format) {
	case "txt", "md", "html", "json", "csv":
		return true
	}
	return false
}
