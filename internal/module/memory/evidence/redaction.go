// Package evidence — redaction gate (design-docs/18 §4.1, §9.1).
//
// Every evidence record runs the redaction gate before it is persisted or
// handed to the ExtractionProvider. The gate is the first line of defense
// against storing secrets and against prompt-injection-via-evidence
// (§9.1: Evidence content is NOT spliced into prompts; the gate emits only
// the minimal redacted fragment the Provider needs).
//
// The gate runs four ordered checks (§4.1):
//  1. Secret/credential detection — a hit REJECTS the capture and audits
//     `evidence.secret_detected` (§4.4). No ciphertext is ever written for a
//     secret-bearing payload.
//  2. PII detection — patterns are masked to `[REDACTED:<kind>]` and the row
//     keeps `classification='pii'`.
//  3. Context trimming — only the slice needed to support the conclusion is
//     retained (11 §8.6). The caller passes the already-trimmed fragment; the
//     gate enforces the size ceiling for the inline path (EvidenceInlineMaxBytes).
//  4. content_hash (SHA-256) over the redacted bytes — the dedup + deletion
//     proof key (§4.1 item 4, §8.4).
//
// The Redactor is pure and side-effect free; persistence + audit are the
// caller's (evidence.Service) job. A secret hit returns ErrSecretDetected so
// the service can map it to a capture-rejected outcome + audit without leaking
// which pattern matched.

package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/lynn901/mora/internal/domain"
)

// ErrSecretDetected is returned by Redact when the input carries a secret or
// credential pattern (§4.1 item 1). The capture MUST be rejected — no
// ciphertext, no excerpt, no storage_key is written for a secret-bearing
// payload. The caller audits `evidence.secret_detected` and surfaces a generic
// rejection so the pattern that matched is never echoed back.
var ErrSecretDetected = errors.New("memory: secret or credential detected in evidence")

// Redaction is the output of Redact: the minimal redacted fragment, its
// SHA-256 content_hash, and the auto-detected classification. RedactedText is
// what gets encrypted/persisted; ContentHash is the dedup + deletion-proof
// key (§8.4). Classification is empty when nothing sensitive was detected
// beyond what was already masked.
type Redaction struct {
	RedactedText  string
	ContentHash   string
	Classification domain.EvidenceClassification
}

// secretPatterns match common credential shapes (§4.1 item 1). A hit is a
// hard rejection — the capture is refused, never partially masked. Patterns
// are deliberately broad and case-insensitive; the goal is to refuse, not to
// precisely classify. They are NOT spliced into prompts or logs — a hit only
// produces a generic ErrSecretDetected.
var secretPatterns = []*regexp.Regexp{
	// Long high-entropy bearer/token shapes (>=32 chars of hex/base64-ish),
	// common in leaked API keys. We require a word-boundary-ish prefix so
	// ordinary prose is not matched.
	regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|passwd|password|auth|bearer|client[_-]?secret)\b[\s:=]{1,3}["']?[A-Za-z0-9_\-\.]{32,}["']?`),
	// AWS access key id / secret access key shapes.
	regexp.MustCompile(`(?i)\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\baws[_-]?(secret[_-]?access[_-]?key)\b[\s:=]{1,3}["']?[A-Za-z0-9/+=]{40}["']?`),
	// PEM private key headers (RSA/EC/OPENSSH/PGP).
	regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----`),
	// GitHub / GitLab personal access token prefixes.
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|glpat-[A-Za-z0-9_\-]{20,})\b`),
	// Slack token prefixes.
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`),
	// JWT shapes (three dot-separated base64url segments, the second decodes
	// to a JSON object with an alg). The raw regex is a coarse shape match.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\b`),
	// Generic "password=<value>" inline.
	regexp.MustCompile(`(?i)\bpassword\b[\s:=]{1,3}\S{4,}`),
	// Connection strings with embedded credentials.
	regexp.MustCompile(`(?i)\b(postgres|mysql|mongodb|redis|amqp)://[^:\s]+:[^@\s]{4,}@`),
}

// piiPatterns map a redaction kind to the pattern that detects it. A hit is
// MASKED, not rejected — the fragment is retained with the sensitive span
// replaced by `[REDACTED:<kind>]` and the row keeps classification='pii'
// (§4.1 item 2).
var piiPatterns = []struct {
	Kind    string
	Pattern *regexp.Regexp
}{
	// Email — mask the local-part, keep nothing identifiable.
	{Kind: "email", Pattern: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	// China mobile (11 digits, common in this stack).
	{Kind: "phone", Pattern: regexp.MustCompile(`\b1[3-9]\d{9}\b`)},
	// E.164 international phone.
	{Kind: "phone", Pattern: regexp.MustCompile(`\+\d{6,15}`)},
	// China resident ID (18 digits, last may be X).
	{Kind: "national_id", Pattern: regexp.MustCompile(`\b\d{17}[\dXx]\b`)},
	// ISO 8601-ish date in an ID-like context is NOT masked (too lossy); only
	// the structural ID shapes above are.
	// Credit card (Pan, 13-19 digits with optional separators).
	{Kind: "card", Pattern: regexp.MustCompile(`\b(?:\d[ -]*?){13,19}\b`)},
}

// Redact runs the §4.1 gate over a candidate evidence fragment and returns the
// redacted text + content_hash + classification. A secret hit returns
// ErrSecretDetected (caller rejects the capture + audits).
//
// The input MUST already be the minimal fragment the caller needs (§4.1 item 3
// / 11 §8.6) — the gate does not re-trim context, it only enforces the inline
// size ceiling and masks/rejects sensitive spans. If the fragment exceeds the
// inline ceiling it is routed to object storage by the caller (§4.2); Redact
// itself stays pure and does not consult the store.
func Redact(fragment string) (Redaction, error) {
	// 1. Secret / credential detection — hard reject (§4.1 item 1).
	for _, p := range secretPatterns {
		if p.MatchString(fragment) {
			return Redaction{}, ErrSecretDetected
		}
	}

	// 2. PII masking (§4.1 item 2).
	redacted := fragment
	classification := domain.EvidenceClassNone
	for _, pp := range piiPatterns {
		if pp.Pattern.MatchString(redacted) {
			redacted = pp.Pattern.ReplaceAllString(redacted, "[REDACTED:"+pp.Kind+"]")
			classification = domain.EvidenceClassPII
		}
	}

	// 4. content_hash over the redacted bytes (§4.1 item 4, §8.4). The hash is
	// computed AFTER masking so it is stable across captures of the same
	// logical content and survives re-ingestion of a redacted fragment.
	sum := sha256.Sum256([]byte(redacted))
	return Redaction{
		RedactedText:   redacted,
		ContentHash:     hex.EncodeToString(sum[:]),
		Classification:  classification,
	}, nil
}

// IsInlineCandidate reports whether a redacted fragment fits the inline
// (encrypted_content) path vs. the object-storage path (§4.2). The ceiling is
// EvidenceInlineMaxBytes (64 KiB). A fragment at or below the ceiling is stored
// as AES-256-GCM ciphertext in memory_evidence.encrypted_content; a larger one
// is written to MinIO under mora-evidence/<workspace>/<evidence_id>.
func IsInlineCandidate(redacted string) bool {
	return len(redacted) <= domain.EvidenceInlineMaxBytes
}

// Excerpt returns a short, human-readable prefix of a redacted fragment for the
// `redacted_excerpt` column (the leak-safe fallback read, §4.3). It is
// rune-counted so it never splits a multibyte sequence (zhparser content).
func Excerpt(redacted string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = 256
	}
	if utf8.RuneCountInString(redacted) <= maxRunes {
		return redacted
	}
	var b strings.Builder
	runeSlice := []rune(redacted)
	b.WriteString(string(runeSlice[:maxRunes]))
	b.WriteString("…")
	return b.String()
}
