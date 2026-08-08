package parser

import (
	"context"
	"encoding/csv"
	"fmt"
	"strings"
)

// CSVParser parses CSV: the header row becomes a section heading; each
// subsequent row becomes a paragraph block that pairs header↔cell so the
// chunker + FTS see "field: value" text per row (§1.3 CSV row).
type CSVParser struct {
	RowsPerChunk int // how many rows to group into one paragraph; 0 = 10
}

func (CSVParser) Name() string { return "csv" }

func (CSVParser) Supports(mime, filename string) bool {
	if mime == "text/csv" || mime == "application/csv" {
		return true
	}
	fn := strings.ToLower(filename)
	return strings.HasSuffix(fn, ".csv")
}

func (p CSVParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	recs, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
	if err != nil {
		return nil, err
	}
	b := newBlockBuilder()
	rowsPer := p.RowsPerChunk
	if rowsPer <= 0 {
		rowsPer = 10
	}
	var header []string
	if len(recs) > 0 {
		header = recs[0]
		if len(recs[0]) > 0 {
			b.heading(2, strings.Join(header, " / "))
		}
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
	for i := 1; i < len(recs); i++ {
		row := recs[i]
		for c := 0; c < len(row); c++ {
			key := fmt.Sprintf("col%d", c+1)
			if c < len(header) && header[c] != "" {
				key = header[c]
			}
			if row[c] != "" {
				if buf.Len() > 0 {
					buf.WriteString(" | ")
				}
				buf.WriteString(key)
				buf.WriteString(": ")
				buf.WriteString(row[c])
			}
		}
		rowInBuf++
		if rowInBuf >= rowsPer {
			flush()
		}
	}
	flush()
	return b.result("csv", p.Name(), nil)
}
