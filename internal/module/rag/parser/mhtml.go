package parser

import (
	"bufio"
	"context"
	"io"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// MhtmlParser parses MHTML (a single-file web archive = multipart/related with
// quoted-printable HTML parts). It uses only the standard library
// (net/mime/multipart + net/textproto, §1.3 MHTML). The first text/html part is
// walked by the shared HTML walker; non-HTML parts are skipped (images, etc.).
type MhtmlParser struct{}

func (MhtmlParser) Name() string { return "mhtml" }

func (MhtmlParser) Supports(mime, filename string) bool {
	fn := strings.ToLower(filename)
	if strings.HasSuffix(fn, ".mhtml") || strings.HasSuffix(fn, ".mht") {
		return true
	}
	return mime == "application/x-mimearchive" || mime == "message/rfc822"
}

func (p MhtmlParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	raw, err := r.Read(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	b := newBlockBuilder()
	// MHTML starts with headers, then a multipart body. We parse it as a
	// multipart message whose boundary is declared in the top-level
	// Content-Type header.
	header, body := splitHeaders(string(raw))
	ct := header.Get("Content-Type")
	if ct == "" || !strings.Contains(ct, "boundary=") {
		// Not a valid MHTML archive; treat the whole thing as HTML/text.
		walkHTMLString(b, string(raw))
		return b.resultWithMeta(ParsedMeta{
			Format: "mhtml", ParserName: p.Name(),
			Warnings: []string{"missing multipart boundary, parsed as HTML"},
		})
	}
	mr := multipart.NewReader(strings.NewReader(body), boundary(ct))
	for {
		part, err := mr.NextPart()
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}
		pct := part.Header.Get("Content-Type")
		mediatype, _, _ := strings.Cut(pct, ";")
		mediatype = strings.ToLower(strings.TrimSpace(mediatype))
		if mediatype == "text/html" || mediatype == "application/xhtml+xml" {
			data, err := io.ReadAll(part)
			if err != nil {
				continue
			}
			walkHTMLString(b, string(data))
		}
	}
	if b.isEmpty() {
		b.paragraph("")
	}
	return b.resultWithMeta(ParsedMeta{Format: "mhtml", ParserName: p.Name()})
}

// splitHeaders separates the leading RFC822-style header block from the body.
func splitHeaders(s string) (textproto.MIMEHeader, string) {
	idx := strings.Index(s, "\r\n\r\n")
	if idx < 0 {
		idx = strings.Index(s, "\n\n")
		if idx < 0 {
			return textproto.MIMEHeader{}, s
		}
		hdr := s[:idx]
		body := s[idx+2:]
		return parseMimeHeader(hdr), body
	}
	hdr := s[:idx]
	body := s[idx+4:]
	return parseMimeHeader(hdr), body
}

// parseMimeHeader parses header lines via textproto (handles folded lines).
func parseMimeHeader(raw string) textproto.MIMEHeader {
	r := textproto.NewReader(bufio.NewReader(strings.NewReader(raw)))
	hdr, _ := r.ReadMIMEHeader()
	if hdr == nil {
		hdr = textproto.MIMEHeader{}
	}
	return hdr
}

// boundary extracts the boundary= value from a Content-Type string.
func boundary(ct string) string {
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "boundary=") {
			v := part[len("boundary="):]
			v = strings.Trim(v, "\"'")
			return v
		}
	}
	return ""
}
