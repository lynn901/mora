package parser

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry routes (mime, filename) to a registered Parser (§1.2). The first
// parser whose Supports() returns true wins; registration order is stable
// (slice, not map) so callers can override by registering later.
type Registry struct {
	mu      sync.RWMutex
	parsers []Parser
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds a parser. Order matters: the first Supports()=true wins.
func (r *Registry) Register(p Parser) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parsers = append(r.parsers, p)
}

// Lookup returns the first parser that handles (mime, filename).
func (r *Registry) Lookup(mime, filename string) (Parser, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// normalize inputs
	mime = strings.ToLower(strings.TrimSpace(mime))
	filename = strings.ToLower(strings.TrimSpace(filename))
	for _, p := range r.parsers {
		if p.Supports(mime, filename) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("parser: no registered parser for mime=%q filename=%q", mime, filename)
}

// List returns the names of registered parsers (for diagnostics/metrics).
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.parsers))
	for i, p := range r.parsers {
		out[i] = p.Name()
	}
	sort.Strings(out)
	return out
}

// FormatFromName infers the canonical source format key (stored in
// documents.source_format) from a filename. Returns "" if unknown. Used by the
// upload path to pre-set source_format before the parser runs, and by tests.
func FormatFromName(filename string) string {
	ext := extOf(filename)
	switch ext {
	case ".txt":
		return "txt"
	case ".md", ".markdown":
		return "md"
	case ".html", ".htm":
		return "html"
	case ".json":
		return "json"
	case ".csv":
		return "csv"
	case ".pdf":
		return "pdf"
	case ".docx":
		return "docx"
	case ".xlsx":
		return "xlsx"
	case ".pptx":
		return "pptx"
	case ".epub":
		return "epub"
	case ".mhtml", ".mht":
		return "mhtml"
	}
	return ""
}

// extOf returns the lowercased extension including the dot, or "".
func extOf(filename string) string {
	filename = strings.ToLower(filename)
	i := strings.LastIndex(filename, ".")
	if i < 0 {
		return ""
	}
	return filename[i:]
}
