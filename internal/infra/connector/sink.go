package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"

	"github.com/lynn901/mora/internal/module/knowledge/source/connector"
	"github.com/lynn901/mora/internal/platform/egress"
)

// MinIOSink is a Mora-provided ContentSink backed by MinIO (§7.1). It writes
// to a per-Run isolated prefix ("source/{run_id}/") and enforces a per-target
// size cap. The sink buffers to memory (Phase 1: sources are doc-sized); a
// future revision can stream multipart uploads for large codebases.
//
// The sink computes the content hash on close (sha256) and returns a
// non-executable Locator (MinIO key under the isolated prefix).
type MinIOSink struct {
	// Prefix is the isolated MinIO key prefix for this Run, e.g. "source/{run_id}/".
	Prefix string
	// MaxBytes caps a single target's content; 0 = no cap (egress size caps
	// still apply upstream for URL/git sources).
	MaxBytes int64
	// Put is the function that uploads bytes to MinIO. The mora-api wires this
	// to objstore.Store.Put. A nil Put makes the sink a no-op store (tests).
	Put func(ctx context.Context, key, contentType string, data []byte) (string, error)
}

// Compile-time check.
var _ connector.ContentSink = (*MinIOSink)(nil)

// Write returns a ContentWriter scoped to targetKey. The writer buffers to
// memory (honoring MaxBytes), hashes on close, and uploads to MinIO on close.
func (s *MinIOSink) Write(ctx context.Context, targetKey string) (connector.ContentWriter, error) {
	if targetKey == "" {
		return nil, errors.New("sink: targetKey required")
	}
	key := s.Prefix + targetKey
	return &minioWriter{
		ctx:    ctx,
		sink:   s,
		key:    key,
		buf:    &bytes.Buffer{},
		hasher: sha256.New(),
	}, nil
}

type minioWriter struct {
	ctx      context.Context
	sink     *MinIOSink
	key      string
	buf      *bytes.Buffer
	hasher  hash.Hash
	hash    string
	loc     connector.Locator
	closed  bool
}

func (w *minioWriter) Write(p []byte) (int, error) {
	if w.sink.MaxBytes > 0 && int64(w.buf.Len())+int64(len(p)) > w.sink.MaxBytes {
		return 0, connector.ErrContentTooLarge
	}
	n, err := w.buf.Write(p)
	if err != nil {
		return n, err
	}
	_, _ = w.hasher.Write(p)
	return n, nil
}

func (w *minioWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.hash = hex.EncodeToString(w.hasher.Sum(nil))
	w.loc = connector.Locator{Kind: "minio", Key: w.key}
	if w.sink.Put != nil {
		if _, err := w.sink.Put(w.ctx, w.key, "application/octet-stream", w.buf.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func (w *minioWriter) Hash() string    { return w.hash }
func (w *minioWriter) Locator() connector.Locator { return w.loc }

// FileSink is a ContentSink that writes to the local filesystem under an
// isolated temp dir (§7.1). Used by tests and by the file adapter when MinIO
// is not configured.
type FileSink struct {
	Dir      string
	MaxBytes int64
}

var _ connector.ContentSink = (*FileSink)(nil)

// Write returns a ContentWriter scoped to targetKey under Dir.
func (s *FileSink) Write(ctx context.Context, targetKey string) (connector.ContentWriter, error) {
	if targetKey == "" {
		return nil, errors.New("sink: targetKey required")
	}
	if s.Dir == "" {
		var err error
		s.Dir, err = os.MkdirTemp("", "mora-sink-*")
		if err != nil {
			return nil, err
		}
	}
	path := s.Dir + "/" + targetKey
	if err := os.MkdirAll(path[:lastSep(path)], 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &fileWriter{f: f, path: path, max: s.MaxBytes, hasher: sha256.New()}, nil
}

type fileWriter struct {
	f       *os.File
	path    string
	max     int64
	hasher hash.Hash
	written int64
	hash    string
	loc     connector.Locator
	closed  bool
}

func (w *fileWriter) Write(p []byte) (int, error) {
	if w.max > 0 && w.written+int64(len(p)) > w.max {
		return 0, connector.ErrContentTooLarge
	}
	n, err := w.f.Write(p)
	w.written += int64(n)
	_, _ = w.hasher.Write(p)
	return n, err
}

func (w *fileWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.f.Close(); err != nil {
		return err
	}
	w.hash = hex.EncodeToString(w.hasher.Sum(nil))
	w.loc = connector.Locator{Kind: "file", Key: w.path}
	return nil
}

func (w *fileWriter) Hash() string    { return w.hash }
func (w *fileWriter) Locator() connector.Locator { return w.loc }

// NoopSink discards content but still computes a hash + a memory locator.
// Used by tests that only need the manifest, not the bytes.
type NoopSink struct{}

var _ connector.ContentSink = (*NoopSink)(nil)

// Write returns a writer that discards bytes and computes a sha256 hash.
func (*NoopSink) Write(_ context.Context, targetKey string) (connector.ContentWriter, error) {
	return &noopWriter{hasher: sha256.New(), key: "noop://" + targetKey}, nil
}

type noopWriter struct {
	hasher hash.Hash
	key    string
	hash   string
	closed bool
}

func (w *noopWriter) Write(p []byte) (int, error) {
	_, _ = w.hasher.Write(p)
	return io.Discard.Write(p)
}

func (w *noopWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.hash = hex.EncodeToString(w.hasher.Sum(nil))
	return nil
}

func (w *noopWriter) Hash() string    { return w.hash }
func (w *noopWriter) Locator() connector.Locator { return connector.Locator{Kind: "noop", Key: w.key} }

// lastSep returns the index of the last path separator, or len(s) if none.
func lastSep(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return len(s)
}

// Compile-time: ensure the egress import is used (HashTargetKey is referenced
// by the adapters; this guards against an unused-import error if an adapter
// file is later trimmed).
var _ = egress.RedactURL
