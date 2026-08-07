package parser

import (
	"context"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// HTMLParser strips tags and preserves the heading hierarchy (h1-h6 → heading
// blocks) so the chunker still gets section boundaries (§1.3 HTML). It uses
// golang.org/x/net/html (BSD-3, no CGO) — not goquery — to avoid a heavy dep.
type HTMLParser struct{}

func (HTMLParser) Name() string { return "html" }

func (HTMLParser) Supports(mime, filename string) bool {
	fn := strings.ToLower(filename)
	if strings.HasSuffix(fn, ".html") || strings.HasSuffix(fn, ".htm") {
		return true
	}
	return mime == "text/html" || mime == "application/xhtml+xml"
}

func (p HTMLParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	b := newBlockBuilder()
	walkHTMLString(b, string(raw))
	return b.result("html", p.Name(), nil)
}

// walkHTMLString parses an HTML string and walks its DOM into b. Shared with
// the EPUB parser (whose sections are XHTML fragments).
func walkHTMLString(b *blockBuilder, htmlSrc string) {
	doc, err := html.Parse(strings.NewReader(htmlSrc))
	if err != nil {
		// fall back to plain text so the content is still searchable
		b.paragraph(collapseWS(htmlSrc))
		return
	}
	walkHTML(b, doc)
}

// walkHTML walks the DOM. Headings (h1-h6) flush the current paragraph and emit
// a heading block; <p>, <li>, <td>, <pre> flush and start a new paragraph; other
// text nodes accumulate into the current paragraph.
func walkHTML(b *blockBuilder, n *html.Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.ElementNode:
		tag := n.Data
		if lvl := headingLevelFor(tag); lvl > 0 {
			text := nodeText(n)
			if strings.TrimSpace(text) != "" {
				b.heading(lvl, text)
				// skip children — already captured as the heading text
				walkHTML(b, n.NextSibling)
				return
			}
		}
		switch tag {
		case "p", "li", "td", "th", "blockquote":
			text := nodeText(n)
			if strings.TrimSpace(text) != "" {
				if tag == "blockquote" {
					b.quote(text)
				} else {
					b.paragraph(text)
				}
				walkHTML(b, n.NextSibling)
				return
			}
		case "pre":
			text := nodeText(n)
			if strings.TrimSpace(text) != "" {
				b.code("", text)
				walkHTML(b, n.NextSibling)
				return
			}
		case "ul":
			items := listItems(n)
			b.list(false, items)
			walkHTML(b, n.NextSibling)
			return
		case "ol":
			items := listItems(n)
			b.list(true, items)
			walkHTML(b, n.NextSibling)
			return
		case "script", "style", "head":
			// skip
			walkHTML(b, n.NextSibling)
			return
		}
	case html.TextNode:
		// text outside a block element: accumulate as a paragraph
		if t := strings.TrimSpace(n.Data); t != "" {
			b.paragraph(t)
		}
	}
	// descend
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTML(b, c)
	}
	walkHTML(b, n.NextSibling)
}

func headingLevelFor(tag string) int {
	if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
		return int(tag[1] - '0')
	}
	return 0
}

// nodeText gathers visible text under a node, collapsing whitespace.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return collapseWS(b.String())
}

func listItems(ul *html.Node) []string {
	var out []string
	for c := ul.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "li" {
			out = append(out, nodeText(c))
		}
	}
	return out
}

func collapseWS(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// keep io referenced for clarity (the parser reads via Reader, not io directly,
// but html.Parse returns no io error path we expose).
var _ = io.EOF
