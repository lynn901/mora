package parser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// memReader is an in-memory parser.Reader for tests: storageKey → bytes.
type memReader map[string][]byte

func (m memReader) Read(ctx context.Context, key string) ([]byte, error) {
	if b, ok := m[key]; ok {
		return b, nil
	}
	return nil, nil
}

// mustParse runs p on raw and fails the test on error.
func mustParse(t *testing.T, p Parser, raw []byte, opts ParseOptions) *ParsedDocument {
	t.Helper()
	r := memReader{"k": raw}
	out, err := p.Parse(context.Background(), r, "k", opts)
	if err != nil {
		t.Fatalf("%s.Parse: %v", p.Name(), err)
	}
	return out
}

// blocksDecode unmarshals the ParsedDocument.Blocks into []map to assert shape.
func blocksDecode(t *testing.T, p *ParsedDocument) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	if err := json.Unmarshal(p.Blocks, &blocks); err != nil {
		t.Fatalf("blocks not valid JSON array: %v", err)
	}
	return blocks
}

func TestTextParser_PlainText(t *testing.T) {
	p := TextParser{}
	out := mustParse(t, p, []byte("Hello world.\n\nSecond paragraph here."), ParseOptions{})
	if out.Meta.Format != "txt" {
		t.Errorf("format = %q want txt", out.Meta.Format)
	}
	if !strings.Contains(out.ContentText, "Hello world") {
		t.Errorf("content_text missing text: %q", out.ContentText)
	}
	blocks := blocksDecode(t, out)
	if len(blocks) < 2 {
		t.Fatalf("expected >=2 blocks, got %d", len(blocks))
	}
}

func TestTextParser_GBKEncoding(t *testing.T) {
	// "中文测试" encoded as GBK should decode to UTF-8.
	gbk := []byte{0xD6, 0xD0, 0xCE, 0xC4, 0xB2, 0xE2, 0xCA, 0xD4}
	p := TextParser{}
	out := mustParse(t, p, gbk, ParseOptions{})
	if !strings.Contains(out.ContentText, "中文测试") {
		t.Errorf("GBK decode failed, got: %q", out.ContentText)
	}
}

func TestMarkdownParser_Headings(t *testing.T) {
	p := MarkdownParser{}
	md := "# Title\n\nIntro paragraph.\n\n## Section\n\nBody text.\n"
	out := mustParse(t, p, []byte(md), ParseOptions{})
	if out.Title != "Title" {
		t.Errorf("title = %q want Title", out.Title)
	}
	if !strings.Contains(out.ContentText, "# Title") {
		t.Errorf("content_text should keep markdown markers for chunker, got: %q", out.ContentText)
	}
	blocks := blocksDecode(t, out)
	if len(blocks) < 3 {
		t.Fatalf("expected >=3 blocks, got %d", len(blocks))
	}
	// first block should be a heading
	if blocks[0]["type"] != "heading" {
		t.Errorf("first block type = %v want heading", blocks[0]["type"])
	}
}

func TestHTMLParser_HeadingsAndParagraphs(t *testing.T) {
	p := HTMLParser{}
	html := `<html><body><h1>Title</h1><p>First paragraph.</p><h2>Sub</h2><p>Second.</p></body></html>`
	out := mustParse(t, p, []byte(html), ParseOptions{})
	if !strings.Contains(out.ContentText, "Title") {
		t.Errorf("missing heading text: %q", out.ContentText)
	}
	blocks := blocksDecode(t, out)
	if len(blocks) < 4 {
		t.Fatalf("expected >=4 blocks (2 headings + 2 paras), got %d", len(blocks))
	}
}

func TestJSONParser_ObjectAndArray(t *testing.T) {
	p := JSONParser{}
	t.Run("object", func(t *testing.T) {
		out := mustParse(t, p, []byte(`{"name":"mora","version":1}`), ParseOptions{})
		if !strings.Contains(out.ContentText, "name") || !strings.Contains(out.ContentText, "mora") {
			t.Errorf("object keys not surfaced: %q", out.ContentText)
		}
	})
	t.Run("array", func(t *testing.T) {
		out := mustParse(t, p, []byte(`[{"a":1},{"a":2}]`), ParseOptions{})
		if !strings.Contains(out.ContentText, "[0]") || !strings.Contains(out.ContentText, "[1]") {
			t.Errorf("array rows not surfaced: %q", out.ContentText)
		}
	})
}

func TestCSVParser_HeaderAndRows(t *testing.T) {
	p := CSVParser{}
	csv := "name,age\nAlice,30\nBob,25\n"
	out := mustParse(t, p, []byte(csv), ParseOptions{})
	if !strings.Contains(out.ContentText, "name: Alice") {
		t.Errorf("header-prefixed row missing: %q", out.ContentText)
	}
	if !strings.Contains(out.ContentText, "age: 30") {
		t.Errorf("cell value missing: %q", out.ContentText)
	}
}

func TestPDFParser_InvalidIsRawText(t *testing.T) {
	p := PDFParser{}
	out := mustParse(t, p, []byte("not a pdf"), ParseOptions{})
	if len(out.Meta.Warnings) == 0 {
		t.Errorf("expected invalid-PDF warning")
	}
	if !strings.Contains(out.ContentText, "not a pdf") {
		t.Errorf("raw text should be indexed: %q", out.ContentText)
	}
}

func TestDocxParser_HeadingsAndParagraphs(t *testing.T) {
	p := DocxParser{}
	// build a minimal docx zip with word/document.xml containing a heading + para
	docx := buildMinimalDocx(t)
	out := mustParse(t, p, docx, ParseOptions{})
	if out.Meta.Format != "docx" {
		t.Errorf("format = %q want docx", out.Meta.Format)
	}
	if !strings.Contains(out.ContentText, "Heading text") {
		t.Errorf("heading text missing: %q", out.ContentText)
	}
	if !strings.Contains(out.ContentText, "paragraph body") {
		t.Errorf("paragraph text missing: %q", out.ContentText)
	}
}

func TestXlsxParser_SheetsAndRows(t *testing.T) {
	p := XlsxParser{}
	xlsx := buildMinimalXlsx(t)
	out := mustParse(t, p, xlsx, ParseOptions{})
	if out.Meta.Format != "xlsx" {
		t.Errorf("format = %q want xlsx", out.Meta.Format)
	}
	if !strings.Contains(out.ContentText, "Sheet:") {
		t.Errorf("sheet heading missing: %q", out.ContentText)
	}
	if !strings.Contains(out.ContentText, "name: Alice") {
		t.Errorf("cell value missing: %q", out.ContentText)
	}
}

func TestEpubParser_SpineOrder(t *testing.T) {
	p := EpubParser{}
	epub := buildMinimalEpub(t)
	out := mustParse(t, p, epub, ParseOptions{})
	if out.Meta.Format != "epub" {
		t.Errorf("format = %q want epub", out.Meta.Format)
	}
	if !strings.Contains(out.ContentText, "Chapter content") {
		t.Errorf("chapter text missing: %q", out.ContentText)
	}
}

func TestMhtmlParser_HTMLPart(t *testing.T) {
	p := MhtmlParser{}
	mhtml := "From: <saved@example.com>\r\n" +
		"Content-Type: multipart/related; boundary=\"BOUND\"\r\n\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<html><body><h1>Title</h1><p>Body</p></body></html>\r\n" +
		"--BOUND--\r\n"
	out := mustParse(t, p, []byte(mhtml), ParseOptions{})
	if !strings.Contains(out.ContentText, "Title") {
		t.Errorf("html part text missing: %q", out.ContentText)
	}
}

func TestPptxParser_SlideText(t *testing.T) {
	p := PptxParser{}
	pptx := buildMinimalPptx(t)
	out := mustParse(t, p, pptx, ParseOptions{})
	if out.Meta.Format != "pptx" {
		t.Errorf("format = %q want pptx", out.Meta.Format)
	}
	if !strings.Contains(out.ContentText, "Slide title") {
		t.Errorf("slide title missing: %q", out.ContentText)
	}
	if !strings.Contains(out.ContentText, "Body text") {
		t.Errorf("slide body missing: %q", out.ContentText)
	}
}

func TestRegistry_RoutesByExtension(t *testing.T) {
	r := DefaultRegistry()
	cases := []struct {
		filename string
		want     string
	}{
		{"readme.md", "markdown"},
		{"page.html", "html"},
		{"data.json", "json"},
		{"rows.csv", "csv"},
		{"report.pdf", "ledongthuc/pdf"},
		{"doc.docx", "docx-self"},
		{"sheet.xlsx", "excelize"},
		{"slides.pptx", "pptx-self"},
		{"book.epub", "epub-self"},
		{"page.mhtml", "mhtml"},
		{"notes.txt", "text"},
	}
	for _, c := range cases {
		got, err := r.Lookup("", c.filename)
		if err != nil {
			t.Errorf("Lookup(%q): %v", c.filename, err)
			continue
		}
		if got.Name() != c.want {
			t.Errorf("Lookup(%q) = %q, want %q", c.filename, got.Name(), c.want)
		}
	}
}

func TestRegistry_UnknownFormatErrors(t *testing.T) {
	r := DefaultRegistry()
	if _, err := r.Lookup("", "file.xyz"); err == nil {
		t.Errorf("expected error for unknown format")
	}
}

func TestFormatFromName(t *testing.T) {
	cases := map[string]string{
		"a.txt":     "txt",
		"b.md":      "md",
		"c.html":    "html",
		"d.pdf":     "pdf",
		"e.docx":    "docx",
		"f.xlsx":    "xlsx",
		"g.pptx":    "pptx",
		"h.epub":    "epub",
		"i.mhtml":   "mhtml",
		"j.unknown": "",
	}
	for name, want := range cases {
		if got := FormatFromName(name); got != want {
			t.Errorf("FormatFromName(%q) = %q, want %q", name, got, want)
		}
	}
}
