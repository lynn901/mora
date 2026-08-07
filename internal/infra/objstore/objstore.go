// Package objstore implements the parser.Reader and the upload-path object
// storage abstraction (design-docs/10 §4.2.1). MinIO/S3-compatible.
//
// To avoid pulling the minio-go SDK (which transitively requires a large
// compression lib that flaked the module proxy at build time), this package
// implements the S3 REST API directly with net/http + AWS SigV4 signing
// (read GET, write PUT). That covers everything the parse pipeline needs:
// upload (PUT), read-back (GET), delete (DELETE). Pure stdlib, no CGO.
package objstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Store is the object-storage abstraction used by both the upload handler
// (PUT a file) and the parser (GET bytes by storage key). It is the
// implementation of parser.Reader (the Read method).
type Store struct {
	Endpoint  string // e.g. http://minio:9000
	AccessKey string
	SecretKey string
	Bucket    string
	Secure    bool // true = HTTPS (production); false = HTTP (local dev)
	Region    string
	HTTP      *http.Client
}

// New returns a Store. secure defaults to false when the endpoint is localhost
// (dev), true otherwise — callers can override.
func New(endpoint, accessKey, secretKey, bucket, region string) *Store {
	s := &Store{
		Endpoint:  strings.TrimRight(endpoint, "/"),
		AccessKey: accessKey,
		SecretKey: secretKey,
		Bucket:    bucket,
		Region:    orDefault(region, "us-east-1"),
		HTTP:      &http.Client{Timeout: 5 * time.Minute},
	}
	return s
}

// EnsureBucket creates the bucket if it does not exist (idempotent). Called at
// startup so uploads don't fail on a fresh MinIO.
func (s *Store) EnsureBucket(ctx context.Context) error {
	if s == nil || s.Endpoint == "" {
		return ErrNotConfigured
	}
	req, err := s.newSignedRequest(ctx, http.MethodPut, "/", nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 200 = created; 409 = already exists (MinIO); either is fine.
	if resp.StatusCode < 300 || resp.StatusCode == http.StatusConflict {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("ensure bucket %s: HTTP %d: %s", s.Bucket, resp.StatusCode, string(body))
}

// Put writes data at key and returns the canonical storage key. The caller
// stores this key on the document row (documents.storage_key).
func (s *Store) Put(ctx context.Context, key string, contentType string, data []byte) (string, error) {
	if s == nil || s.Endpoint == "" {
		return "", ErrNotConfigured
	}
	hdr := map[string]string{
		"Content-Type":   orDefault(contentType, "application/octet-stream"),
		"Content-Length": fmt.Sprintf("%d", len(data)),
	}
	req, err := s.newSignedRequest(ctx, http.MethodPut, "/"+key, data, hdr)
	if err != nil {
		return "", err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("put %s: HTTP %d: %s", key, resp.StatusCode, string(body))
	}
	return key, nil
}

// Read implements parser.Reader: fetches the bytes at storageKey. Used by every
// parser to read the uploaded file.
func (s *Store) Read(ctx context.Context, storageKey string) ([]byte, error) {
	if s == nil || s.Endpoint == "" {
		return nil, ErrNotConfigured
	}
	req, err := s.newSignedRequest(ctx, http.MethodGet, "/"+storageKey, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("object %s not found", storageKey)
		}
		return nil, fmt.Errorf("get %s: HTTP %d: %s", storageKey, resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

// Delete removes an object (best-effort; used on document delete cascade).
func (s *Store) Delete(ctx context.Context, key string) error {
	if s == nil || s.Endpoint == "" {
		return ErrNotConfigured
	}
	req, err := s.newSignedRequest(ctx, http.MethodDelete, "/"+key, nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete %s: HTTP %d", key, resp.StatusCode)
	}
	return nil
}

// ErrNotConfigured is returned when the store endpoint is empty (object
// storage not wired). Callers should gate on this (e.g. fall back to local FS
// or reject uploads) rather than crash.
var ErrNotConfigured = errors.New("objstore: not configured (empty endpoint)")

// --- AWS SigV4 signing (minimal, S3-compatible) ---
//
// S3 SigV4 signs a canonical request with an HMAC-SHA256 derived signing key.
// This is the same algorithm MinIO accepts; we implement the "streaming-unaware"
// (single-chunk) variant, which is all the parse pipeline needs.

func (s *Store) newSignedRequest(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Request, error) {
	u, err := url.Parse(s.Endpoint + path)
	if err != nil {
		return nil, err
	}
	// object key path may contain spaces/unicode; encode each segment.
	u.RawQuery = ""
	var segs []string
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		seg, _ = url.QueryUnescape(seg)
		segs = append(segs, url.PathEscape(seg))
	}
	encPath := "/" + strings.Join(segs, "/")
	full := s.Endpoint + encPath

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, bodyReader)
	if err != nil {
		return nil, err
	}
	if hdr != nil {
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
	}
	// x-amz-content-sha256 of the body (empty string → SHA256 of "").
	req.Header.Set("x-amz-content-sha256", sha256Hex(body))
	req.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	host := req.URL.Hostname()
	if p := req.URL.Port(); p != "" {
		host += ":" + p
	}
	req.Host = host
	if err := signSigV4(req, body, s.AccessKey, s.SecretKey, s.Region); err != nil {
		return nil, err
	}
	return req, nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func signSigV4(req *http.Request, body []byte, accessKey, secretKey, region string) error {
	t := req.Header.Get("x-amz-date")
	if t == "" {
		return errors.New("missing x-amz-date")
	}
	date, err := time.Parse("20060102T150405Z", t)
	if err != nil {
		return err
	}
	shortDate := date.Format("20060102")
	bodyHash := sha256Hex(body)

	// canonical headers (host + x-amz-content-sha256 + x-amz-date, lowercase, sorted)
	hdrs := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	signedHdrs := strings.Join(hdrs, ";")
	canonicalHdrs := "host:" + req.Host + "\n" +
		"x-amz-content-sha256:" + bodyHash + "\n" +
		"x-amz-date:" + t + "\n"

	// canonical query string (none for our requests)
	canonicalReq := req.Method + "\n" + req.URL.EscapedPath() + "\n\n" + canonicalHdrs + "\n" + signedHdrs + "\n" + bodyHash

	scope := shortDate + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + t + "\n" + scope + "\n" + sha256Hex([]byte(canonicalReq))

	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(shortDate))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	auth := "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope + ", SignedHeaders=" + signedHdrs + ", Signature=" + signature
	req.Header.Set("Authorization", auth)
	return nil
}

// s3Error is the XML body MinIO returns on failure (for diagnostics).
type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// sortedHeaderKeys returns the header keys lowercased and sorted (SigV4 canonical).
func sortedHeaderKeys(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, strings.ToLower(k))
	}
	sort.Strings(out)
	return out
}
