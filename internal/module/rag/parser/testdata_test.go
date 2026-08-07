package parser

import (
	"archive/zip"
	"bytes"
	"testing"

	"github.com/xuri/excelize/v2"
)

// buildMinimalDocx builds a tiny .docx zip with word/document.xml containing a
// Heading1 paragraph and a normal paragraph. The OOXML is hand-rolled to the
// minimum the docx parser reads (w:p / w:pStyle / w:t).
func buildMinimalDocx(t *testing.T) []byte {
	t.Helper()
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Heading text</w:t></w:r></w:p>
    <w:p><w:r><w:t>This is a paragraph body.</w:t></w:r></w:p>
  </w:body>
</w:document>`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := w.Write([]byte(doc)); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// buildMinimalXlsx builds a tiny .xlsx with one sheet "Sheet1" and two rows
// (header + data) via the excelize library (so the XML is schema-valid).
func buildMinimalXlsx(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	f.SetSheetName(f.GetSheetName(0), "Sheet1")
	f.SetCellValue("Sheet1", "A1", "name")
	f.SetCellValue("Sheet1", "B1", "age")
	f.SetCellValue("Sheet1", "A2", "Alice")
	f.SetCellValue("Sheet1", "B2", 30)
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("write xlsx: %v", err)
	}
	return buf.Bytes()
}

// buildMinimalEpub builds a tiny .epub zip: META-INF/container.xml pointing at
// content.opf, which lists one spine item (chapter1.xhtml) with a heading +
// paragraph. The walker extracts both.
func buildMinimalEpub(t *testing.T) []byte {
	t.Helper()
	container := `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`
	opf := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Test Book</dc:title>
  </metadata>
  <manifest>
    <item id="ch1" href="chapter1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="ch1"/></spine>
</package>`
	chapter := `<?xml version="1.0"?>
<html xmlns="http://www.w3.org/1999/xhtml"><body>
<h1>Chapter Title</h1><p>Chapter content here.</p>
</body></html>`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range [][2]string{
		{"META-INF/container.xml", container},
		{"content.opf", opf},
		{"chapter1.xhtml", chapter},
	} {
		w, err := zw.Create(entry[0])
		if err != nil {
			t.Fatalf("create %s: %v", entry[0], err)
		}
		if _, err := w.Write([]byte(entry[1])); err != nil {
			t.Fatalf("write %s: %v", entry[0], err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// buildMinimalPptx builds a tiny .pptx with one slide whose shapes carry text.
func buildMinimalPptx(t *testing.T) []byte {
	t.Helper()
	slide := `<?xml version="1.0" encoding="UTF-8"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:cSld><p:spTree>
    <p:sp><p:txBody><a:p><a:r><a:t>Slide title</a:t></a:r></a:p></p:txBody></p:sp>
    <p:sp><p:txBody><a:p><a:r><a:t>Body text</a:t></a:r></a:p></p:txBody></p:sp>
  </p:spTree></p:cSld>
</p:sld>`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("ppt/slides/slide1.xml")
	if err != nil {
		t.Fatalf("create slide: %v", err)
	}
	if _, err := w.Write([]byte(slide)); err != nil {
		t.Fatalf("write slide: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}
