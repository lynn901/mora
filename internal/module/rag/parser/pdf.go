package parser

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDFParser extracts text from the text layer of a PDF (§1.3 PDF 文本层). It
// uses ledongthuc/pdf (BSD-3, pure Go, no CGO). For scanned/complex-layout PDFs
// it produces only what the text layer yields; rich layout + OCR is the P2
// mora-parser sidecar path (§3), intentionally not this parser's job.
type PDFParser struct{}

func (PDFParser) Name() string { return "ledongthuc/pdf" }

func (PDFParser) Supports(mime, filename string) bool {
	fn := strings.ToLower(filename)
	if strings.HasSuffix(fn, ".pdf") {
		return true
	}
	return mime == "application/pdf"
}

func (p PDFParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	b := newBlockBuilder()
	var warnings []string

	reader, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		// Not a valid PDF: keep raw as a paragraph so the upload is searchable.
		b.paragraph(string(raw))
		return b.resultWithMeta(ParsedMeta{
			Format: "pdf", ParserName: p.Name(),
			Warnings: []string{"invalid PDF, indexed as raw text: " + err.Error()},
		})
	}
	pageCount := reader.NumPage()
	// Reader.GetPlainText returns the concatenated text of all pages. Per-page
	// heading boundaries are best-effort: we split on form-feed (\f, which the
	// library inserts between pages) to recover page runs.
	txtReader, err := reader.GetPlainText()
	if err != nil {
		warnings = append(warnings, "get plain text: "+err.Error())
	} else {
		txtBytes, err := io.ReadAll(txtReader)
		if err != nil {
			warnings = append(warnings, "read plain text: "+err.Error())
		}
		pages := strings.Split(string(txtBytes), "\f")
		for i, pg := range pages {
			pg = strings.TrimSpace(pg)
			if pg == "" {
				continue
			}
			b.heading(2, "Page "+itoa(i+1))
			for _, para := range strings.Split(pg, "\n\n") {
				para = strings.TrimSpace(para)
				if para != "" {
					b.paragraph(para)
				}
			}
		}
	}
	if b.isEmpty() {
		// text layer empty → likely a scanned PDF; surface a warning so the
		// caller/UI can route to the OCR sidecar (P2).
		warnings = append(warnings, "no text layer; scanned PDF requires OCR (enable_ocr / mora-parser)")
	}
	return b.resultWithMeta(ParsedMeta{
		Format: "pdf", ParserName: p.Name(), PageCount: pageCount, Warnings: warnings,
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
