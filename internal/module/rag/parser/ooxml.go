package parser

// ooxml.go holds shared helpers for the self-built OOXML parsers (docx, pptx).
// These formats are zip+xml (the Office Open schema). The design (§1.3, §1.5)
// chose self-built readers (~300 lines) over the AGPL unioffice so the main
// process stays License-clean; complex layout is a P2 sidecar concern.

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"strings"
)

// ooxmlZip opens a zip archive from raw bytes.
func ooxmlZip(raw []byte) (*zip.Reader, error) {
	r, err := zip.NewReader(bytesReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	return r, nil
}

// readZipFile returns the bytes of the first zip entry whose name matches name.
func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, nil
}

// eachZipFile runs fn for each entry whose name has the given suffix.
func eachZipFile(zr *zip.Reader, suffix string, fn func(name string, raw []byte) error) error {
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, suffix) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		if err := fn(f.Name, raw); err != nil {
			return err
		}
	}
	return nil
}

// xmlToken is a minimal token used to walk OOXML content runs. We only need
// element start/end and char data — a full xml.Name mapping is overkill.
type xmlWalker struct {
	dec *xml.Decoder
}

// collectXMLText reads every CharData under the current element, recursing into
// nested <w:t>/<a:t> runs, and returns the concatenated text. This is enough to
// extract visible text from OOXML body parts without modeling the full schema.
func collectXMLText(raw []byte) string {
	dec := xml.NewDecoder(bytesReader(raw))
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.CharData:
			s := strings.TrimSpace(string(t))
			if s != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(s)
			}
		}
	}
	return collapseWSInner(b.String())
}

func collapseWSInner(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
