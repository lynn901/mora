package parser

import (
	"context"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

// TextParser handles plain-text uploads (txt): auto-detects GBK/Big5/UTF-8 and
// splits into paragraphs + heading-ish lines (a line that is short, ends no
// punctuation, and is followed by a blank line becomes a heading, best-effort).
type TextParser struct{}

func (TextParser) Name() string { return "text" }

func (TextParser) Supports(mime, filename string) bool {
	if mime == "text/plain" || strings.HasSuffix(filename, ".txt") {
		return true
	}
	return false
}

func (p TextParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	text := decodeText(raw)
	b := newBlockBuilder()
	// A simple heuristic: split on blank lines into paragraphs; a "paragraph"
	// that is short and looks like a title becomes a level-2 heading.
	for _, para := range splitParagraphs(text) {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if isHeadingLine(para) {
			b.heading(2, para)
			continue
		}
		b.paragraph(para)
	}
	return b.result("txt", p.Name(), nil)
}

// decodeText tries UTF-8 first; if invalid, attempts common CJK encodings
// (GBK/Big5) via ianaindex so uploaded Chinese text isn't mojibake.
func decodeText(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	for _, name := range []string{"GBK", "Big5", "Shift_JIS", "EUC-KR", "ISO-8859-1"} {
		enc, err := ianaindex.IANA.Encoding(name)
		if err != nil || enc == nil {
			continue
		}
		dec := enc.NewDecoder()
		out, _, err := transform.String(dec, string(raw))
		if err == nil && utf8.ValidString(out) {
			return out
		}
	}
	return string(raw)
}

func isValidUTF8(b []byte) bool {
	return utf8.Valid(b)
}

func splitParagraphs(s string) []string {
	return strings.Split(s, "\n\n")
}

// isHeadingLine: a short line (≤60 runes), no sentence terminator, not starting
// with a bullet/number. Best-effort; the chunker treats headings as boundaries.
func isHeadingLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if len([]rune(line)) > 60 {
		return false
	}
	if strings.ContainsAny(line, "。.！!？?；;") {
		return false
	}
	if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "•") {
		return false
	}
	if isDigit(line[0]) && strings.Contains(line, ".") {
		return false // numbered list, not a heading
	}
	return true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
