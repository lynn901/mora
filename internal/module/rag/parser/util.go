package parser

import "bytes"

// bytesReader returns a *bytes.Reader for raw. A small wrapper so the various
// zip/xml/parsers share one call site (and tests can pass bytes).
func bytesReader(raw []byte) *bytes.Reader { return bytes.NewReader(raw) }
