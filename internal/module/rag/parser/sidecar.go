package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SidecarParser delegates parsing to the optional mora-parser Python sidecar
// (§1.4). It is the P2 multimodal/complex-layout path: OCR (PaddleOCR/surya),
// VLM image description (Ollama), ASR (whisper.cpp), and rich-layout PDF.
//
// When the sidecar is not configured (MORA_PARSER_URL empty) the SidecarParser
// returns ErrSidecarDisabled so the caller can fall back to the inline pure-Go
// parser — multimodal is opt-in, never a hard dependency of the main pipeline.
type SidecarParser struct {
	BaseURL  string       // e.g. http://mora-parser:8000; "" = disabled
	HTTP     *http.Client // injected for tests; defaults to a 5-min client
	Password string       // optional shared secret (X-Sidecar-Token); "" = none
}

func NewSidecarParser(baseURL string) *SidecarParser {
	s := &SidecarParser{BaseURL: strings.TrimRight(baseURL, "/")}
	s.HTTP = &http.Client{Timeout: 5 * time.Minute}
	return s
}

func (s *SidecarParser) Name() string { return "mora-parser" }

// ErrSidecarDisabled is returned when the sidecar URL is empty.
var ErrSidecarDisabled = errors.New("parser: mora-parser sidecar disabled")

// sidecarRequest is the body POSTed to the sidecar /parse route.
type sidecarRequest struct {
	StorageKey string       `json:"storage_key"`
	MIME       string       `json:"mime,omitempty"`
	Filename   string       `json:"filename,omitempty"`
	Opts       ParseOptions `json:"opts"`
}

// Supports returns true only for formats the inline pure-Go path can't handle
// well: scanned PDFs, images needing OCR/VLM, and audio/video needing ASR.
// When multimodal opts are off, the inline parser is preferred. The registry
// is consulted in registration order, so SidecarParser must be registered FIRST
// (before the inline PDF/text parsers) and only claim a file when multimodal
// opts request it.
func (s *SidecarParser) Supports(mime, filename string) bool {
	if s.BaseURL == "" {
		return false
	}
	fn := strings.ToLower(filename)
	mime = strings.ToLower(mime)
	// Images and audio/video are sidecar-only in P2.
	if strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/") {
		return true
	}
	if strings.HasSuffix(fn, ".png") || strings.HasSuffix(fn, ".jpg") || strings.HasSuffix(fn, ".jpeg") ||
		strings.HasSuffix(fn, ".gif") || strings.HasSuffix(fn, ".bmp") || strings.HasSuffix(fn, ".webp") ||
		strings.HasSuffix(fn, ".mp3") || strings.HasSuffix(fn, ".wav") || strings.HasSuffix(fn, ".mp4") {
		return true
	}
	return false
}

// SupportsOpts reports whether the per-upload opts request the sidecar
// (enable_ocr/vlm/asr/graph/qagen). Used by the orchestrator to decide routing
// for PDFs that could be handled inline OR by the sidecar.
func SupportsOpts(o ParseOptions) bool {
	return o.EnableOCR || o.EnableVLM || o.EnableASR || o.EnableGraph || o.EnableQAGen
}

func (s *SidecarParser) Parse(ctx context.Context, r Reader, storageKey string, opts ParseOptions) (*ParsedDocument, error) {
	if s.BaseURL == "" {
		return nil, ErrSidecarDisabled
	}
	body, err := json.Marshal(sidecarRequest{StorageKey: storageKey, Opts: opts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/parse", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.Password != "" {
		req.Header.Set("X-Sidecar-Token", s.Password)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sidecar parse: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sidecar parse HTTP %d: %s", resp.StatusCode, string(errBytes))
	}
	var out ParsedDocument
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("sidecar parse decode: %w", err)
	}
	if out.Meta.ParserName == "" {
		out.Meta.ParserName = s.Name()
	}
	return &out, nil
}

// DescribeImage calls the sidecar /describe route (VLM image caption, §3.2).
// Returns the description text; empty if the sidecar is disabled/unavailable.
func (s *SidecarParser) DescribeImage(ctx context.Context, storageKey, model string) (string, error) {
	if s.BaseURL == "" {
		return "", ErrSidecarDisabled
	}
	body, _ := json.Marshal(map[string]string{"image_key": storageKey, "model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/describe", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("sidecar describe HTTP %d", resp.StatusCode)
	}
	var out struct {
		Description string `json:"description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Description, nil
}

// OCRImage calls the sidecar /ocr route. Returns extracted text.
func (s *SidecarParser) OCRImage(ctx context.Context, storageKey, lang string) (string, error) {
	if s.BaseURL == "" {
		return "", ErrSidecarDisabled
	}
	body, _ := json.Marshal(map[string]string{"image_key": storageKey, "lang": lang})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/ocr", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("sidecar ocr HTTP %d", resp.StatusCode)
	}
	var out struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Text, nil
}
