package parser

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"io"
	"sort"
	"strings"
)

// EpubParser parses EPUB by self-building a reader (§1.3 EPUB = zip of XHTML,
// ~150 lines). go-shiori/go-epub is a writer, not a reader, so it can't parse.
// We open the zip, read the OPF manifest to learn the spine order, then walk
// each XHTML part with the shared HTML walker so headings/paragraphs carry
// section boundaries into the chunker.
type EpubParser struct{}

func (EpubParser) Name() string { return "epub-self" }

func (EpubParser) Supports(mime, filename string) bool {
	fn := strings.ToLower(filename)
	if strings.HasSuffix(fn, ".epub") {
		return true
	}
	return mime == "application/epub+zip"
}

func (p EpubParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytesReader(raw), int64(len(raw)))
	if err != nil {
		b := newBlockBuilder()
		b.paragraph(string(raw))
		return b.resultWithMeta(ParsedMeta{
			Format: "epub", ParserName: p.Name(),
			Warnings: []string{"invalid epub zip, indexed as raw text: " + err.Error()},
		})
	}
	b := newBlockBuilder()
	title, spine := readEpubOPF(zr)
	if title != "" {
		b.heading(1, title)
	}
	// Walk each spine XHTML part in order.
	for _, href := range spine {
		htmlSrc := readZipEntry(zr, href)
		if htmlSrc == "" {
			continue
		}
		walkHTMLString(b, htmlSrc)
	}
	return b.resultWithMeta(ParsedMeta{Format: "epub", ParserName: p.Name()})
}

// readEpubOPF locates the .opf file via container.xml, parses metadata
// (dc:title) and the spine order (idref → manifest href), and returns the
// ordered list of XHTML hrefs.
func readEpubOPF(zr *zip.Reader) (title string, spine []string) {
	container := readZipEntry(zr, "META-INF/container.xml")
	opfPath := xmlAttr(container, "full-path")
	if opfPath == "" {
		return "", nil
	}
	opf := readZipEntry(zr, opfPath)
	// manifest id → href
	manifest := map[string]string{}
	dec := xml.NewDecoder(strings.NewReader(opf))
	var spineIDs []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "item":
			id, href := "", ""
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "id":
					id = a.Value
				case "href":
					href = a.Value
				}
			}
			if id != "" && href != "" {
				manifest[id] = resolveOPFPath(opfPath, href)
			}
		case "itemref":
			for _, a := range se.Attr {
				if a.Name.Local == "idref" {
					spineIDs = append(spineIDs, a.Value)
				}
			}
		case "title":
			// dc:title — read its CharData
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if cd, ok := tok.(xml.CharData); ok {
				if title == "" {
					title = strings.TrimSpace(string(cd))
				}
			}
		}
	}
	for _, id := range spineIDs {
		if href, ok := manifest[id]; ok {
			spine = append(spine, href)
		}
	}
	return title, spine
}

// xmlAttr returns the value of the named attribute on the first element whose
// name matches elem in a tiny scan of the XML. Used for container.xml's
// rootfile full-path.
func xmlAttr(raw, attr string) string {
	dec := xml.NewDecoder(strings.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == attr {
				return a.Value
			}
		}
	}
}

// resolveOPFPath resolves an href relative to the OPF file's path inside the zip.
func resolveOPFPath(opfPath, href string) string {
	if strings.Contains(href, "/") {
		return href
	}
	dir := opfPath
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		return dir[:i+1] + href
	}
	return href
}

// readZipEntry returns the string content of a zip entry, "" if absent.
func readZipEntry(zr *zip.Reader, name string) string {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return ""
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				return ""
			}
			return string(b)
		}
	}
	return ""
}

// sortedZipNames returns sorted entry names (diagnostics/tests).
func sortedZipNames(zr *zip.Reader) []string {
	out := make([]string, len(zr.File))
	for i, f := range zr.File {
		out[i] = f.Name
	}
	sort.Strings(out)
	return out
}
