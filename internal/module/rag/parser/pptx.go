package parser

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
)

// PptxParser is a self-built OOXML reader for PowerPoint .pptx (§1.3, §1.5).
// pptx is a zip of XML; each ppt/slides/slideN.xml holds <p:sp> shapes whose
// <a:t> runs carry visible text. Each slide becomes a heading + its text as
// paragraphs, mirroring the page-heading structure the PDF parser uses.
type PptxParser struct{}

func (PptxParser) Name() string { return "pptx-self" }

func (PptxParser) Supports(mime, filename string) bool {
	fn := strings.ToLower(filename)
	if strings.HasSuffix(fn, ".pptx") {
		return true
	}
	return mime == "application/vnd.openxmlformats-officedocument.presentationml.presentation"
}

func (p PptxParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	zr, err := ooxmlZip(raw)
	if err != nil {
		b := newBlockBuilder()
		b.paragraph(string(raw))
		return b.resultWithMeta(ParsedMeta{
			Format: "pptx", ParserName: p.Name(),
			Warnings: []string{"invalid pptx zip, indexed as raw text: " + err.Error()},
		})
	}
	b := newBlockBuilder()
	slideNo := 0
	// slides are ppt/slides/slide1.xml, slide2.xml, ...
	// iterate by index since eachZipFile's map order isn't numeric.
	for i := 1; ; i++ {
		name := fmt.Sprintf("ppt/slides/slide%d.xml", i)
		raw, err := readZipFile(zr, name)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			break
		}
		slideNo = i
		text := extractSlideText(raw)
		if strings.TrimSpace(text) == "" {
			continue
		}
		b.heading(2, "Slide "+itoa(i))
		for _, para := range strings.Split(text, "\n") {
			para = strings.TrimSpace(para)
			if para != "" {
				b.paragraph(para)
			}
		}
	}
	return b.resultWithMeta(ParsedMeta{Format: "pptx", ParserName: p.Name(), PageCount: slideNo})
}

// extractSlideText walks a slide's XML and concatenates every <a:t> run's
// text, newline-separated so the caller can split into paragraphs per shape.
func extractSlideText(raw []byte) string {
	dec := xml.NewDecoder(bytesReader(raw))
	var b strings.Builder
	var inT bool
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				inT = true
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				inT = false
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inT {
				b.Write(t)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
