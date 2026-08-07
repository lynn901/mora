package parser

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

// XlsxParser parses Excel .xlsx via excelize (BSD-3, pure Go, §1.3). Each sheet
// becomes a section (heading); rows are flattened to "header: cell" paragraphs
// grouped N rows at a time so each chunk carries row context.
type XlsxParser struct {
	RowsPerChunk int
}

func (XlsxParser) Name() string { return "excelize" }

func (XlsxParser) Supports(mime, filename string) bool {
	fn := strings.ToLower(filename)
	if strings.HasSuffix(fn, ".xlsx") {
		return true
	}
	return mime == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
}

func (p XlsxParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		b := newBlockBuilder()
		b.paragraph(string(raw))
		return b.resultWithMeta(ParsedMeta{
			Format: "xlsx", ParserName: p.Name(),
			Warnings: []string{"invalid xlsx, indexed as raw text: " + err.Error()},
		})
	}
	defer f.Close()
	rowsPer := p.RowsPerChunk
	if rowsPer <= 0 {
		rowsPer = 10
	}
	b := newBlockBuilder()
	for _, sheet := range f.GetSheetList() {
		b.heading(2, "Sheet: "+sheet)
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		var header []string
		if len(rows) > 0 {
			header = rows[0]
		}
		var buf strings.Builder
		rowInBuf := 0
		flush := func() {
			if buf.Len() > 0 {
				b.paragraph(buf.String())
				buf.Reset()
				rowInBuf = 0
			}
		}
		for i := 1; i < len(rows); i++ {
			row := rows[i]
			for c := 0; c < len(row); c++ {
				if strings.TrimSpace(row[c]) == "" {
					continue
				}
				key := fmt.Sprintf("col%d", c+1)
				if c < len(header) && header[c] != "" {
					key = header[c]
				}
				if buf.Len() > 0 {
					buf.WriteString(" | ")
				}
				buf.WriteString(key)
				buf.WriteString(": ")
				buf.WriteString(row[c])
			}
			rowInBuf++
			if rowInBuf >= rowsPer {
				flush()
			}
		}
		flush()
	}
	return b.resultWithMeta(ParsedMeta{Format: "xlsx", ParserName: p.Name()})
}
