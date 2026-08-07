package parser

import (
	"bytes"
	"context"
	"encoding/xml"
	"strings"
)

// DocxParser is a self-built OOXML reader for Word .docx files (§1.3, §1.5).
// docx is a zip of XML; word/document.xml holds the body as <w:p> paragraphs
// containing <w:r><w:t> text runs. We detect headings by the paragraph style
// (<w:pStyle w:val="Heading1"/>) so section boundaries survive into the chunker.
//
// Why self-built, not a library: the only full-featured Go docx lib
// (unidoc/unioffice) is AGPLv3 — putting it in the main process would impose
// copyleft on the whole binary (§1.5). A ~100-line reader covers text + heading
// structure, which is what the RAG pipeline needs; rich layout is a P2 sidecar.
type DocxParser struct{}

func (DocxParser) Name() string { return "docx-self" }

func (DocxParser) Supports(mime, filename string) bool {
	fn := strings.ToLower(filename)
	if strings.HasSuffix(fn, ".docx") {
		return true
	}
	return mime == "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
}

func (p DocxParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	zr, err := ooxmlZip(raw)
	if err != nil {
		b := newBlockBuilder()
		b.paragraph(string(raw))
		return b.resultWithMeta(ParsedMeta{
			Format: "docx", ParserName: p.Name(),
			Warnings: []string{"invalid docx zip, indexed as raw text: " + err.Error()},
		})
	}
	docXML, err := readZipFile(zr, "word/document.xml")
	if err != nil {
		return nil, err
	}
	b := newBlockBuilder()
	walkDocxBody(b, docXML)
	return b.resultWithMeta(ParsedMeta{Format: "docx", ParserName: p.Name()})
}

// walkDocxBody iterates <w:p> paragraphs. A paragraph's style (Heading1-6)
// decides heading vs paragraph; <w:t> runs are concatenated.
func walkDocxBody(b *blockBuilder, raw []byte) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "p" {
			continue
		}
		level, text := readDocxParagraph(dec)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if level > 0 {
			b.heading(level, text)
		} else {
			b.paragraph(text)
		}
	}
}

// readDocxParagraph reads until the matching </w:p>, extracting the pStyle
// level and every <w:t> run's text.
func readDocxParagraph(dec *xml.Decoder) (level int, text string) {
	var b strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return 0, b.String()
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "pStyle" {
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						level = docxHeadingLevel(a.Value)
					}
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			s := string(t)
			if strings.TrimSpace(s) != "" {
				b.WriteString(s)
			}
		}
	}
	return level, b.String()
}

// docxHeadingLevel maps a pStyle val ("Heading1".."Heading6", or "1".."6" in
// some templates) to a heading level; returns 0 for non-heading styles.
func docxHeadingLevel(val string) int {
	val = strings.ToLower(strings.TrimSpace(val))
	if strings.HasPrefix(val, "heading") {
		// "heading1".."heading6"
		tail := val[len("heading"):]
		if len(tail) == 1 && tail[0] >= '1' && tail[0] <= '6' {
			return int(tail[0] - '0')
		}
		return 0
	}
	// numbered heading styles (some templates)
	if len(val) == 1 && val[0] >= '1' && val[0] <= '6' {
		return int(val[0] - '0')
	}
	return 0
}
