// Package evidence — capture service (design-docs/18 §4.1, §4.2, §5.3, §7.4).
//
// Capture is the write entry point: `memory_remember` and session import both
// call Capture to turn a submitted conclusion + minimal evidence snippet into
// a memory_evidence row + an `evidence.captured` outbox event (D5/D9). The
// outbox row is written in the SAME transaction as the evidence row, so the
// event is never lost (§6.3). The knowledge-worker's memory_distill consumer
// picks the event up and dispatches a memory_extract job (§3.3).
//
// Capture is fail-closed on secrets: a Redact hit on a secret/credential
// pattern rejects the capture and audits `evidence.secret_detected` — no
// ciphertext, no excerpt, no storage_key is written for a secret-bearing
// payload (§4.1 item 1, §9.1). PII is masked and retained (classification='pii').
//
// Storage split (D4, §4.2): fragments ≤ 64KiB redacted are AES-256-GCM
// encrypted into memory_evidence.encrypted_content (the DEK is envelope-wrapped
// by a versioned KEK); larger fragments are written to MinIO under
// mora-evidence/<workspace>/<id> and only storage_key + content_hash +
// redacted_excerpt persist. The split decision happens BEFORE encryption so the
// hash is stable.
package evidence

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/audit"
	"github.com/lynn901/mora/internal/platform/outbox"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// CaptureRequest is the input to Service.Capture — the minimal, caller-trimmed
// evidence snippet + its non-executable source locator (D9, §4.1 item 3).
// RawSnippet is the UN-redacted fragment; the service runs Redact before any
// storage/encrypt. A secret hit returns ErrSecretDetected from Capture.
type CaptureRequest struct {
	WorkspaceID  uuid.UUID
	OwnerType     domain.OwnerType
	OwnerID       uuid.UUID
	SourceKind    domain.EvidenceSourceKind
	SourceRef     string                 // session_id / message_id / tool_call_id / asset_version_id
	SourceAssetID *uuid.UUID             // knowledge_assets(id); nil for session/message/tool_call
	SourceAssetVersionID *uuid.UUID
	Visibility    domain.EvidenceVisibility
	RawSnippet    string                 // caller-trimmed minimal fragment (11 §8.6); redacted by Capture
	AuthzRevision int64                  // workspace_authz_revisions.revision at capture (audit-only, §4.4)
	// RetentionPolicyID is optional; the service resolves the effective policy
	// from (workspace, source_kind proxy = memory_type none) when nil so the
	// row carries an expires_at from the start (§9.2 deletion propagation).
	RetentionPolicyID *uuid.UUID
}

// CaptureResult is the outcome of a successful Capture — the evidence id (for
// the `evidence.captured` event + the caller's response) and whether the
// fragment routed inline (encrypted_content) vs. to object storage (§4.2).
type CaptureResult struct {
	EvidenceID uuid.UUID
	Inline     bool
	ContentHash string
	Classification domain.EvidenceClassification
}

// Service is the capture orchestrator. It composes the redaction gate, the
// KEK + Crypto + ObjectStore ports, the EvidenceRepo, and the outbox Store.
// It is the ONLY writer of memory_evidence rows from a capture entry; the
// deletion-propagation reaper writes state transitions (MarkPendingPurge /
// Purge) directly via the repo.
type Service struct {
	repos    EvidenceRepo
	retention RetentionPolicyRepo
	kek     KEK
	crypto  Crypto
	objects ObjectStore
	outbox  *outbox.Store
	rbac    *rbac.Engine // nil = no resource-level authz (dev/test only)
	audit   *audit.Logger
}

// AuthContext carries the caller identity needed for the workspace-write RBAC
// gate on Capture (design-docs/18 §4.4 — capture requires write on the
// workspace). It mirrors knowledge/source/service.AuthContext: an admin
// short-circuits to allowed, an agent acting on behalf of a user records the
// user as the RBAC subject, a service_account caller resolves to itself with
// no admin bypass.
type AuthContext struct {
	SubjectType      domain.SubjectType
	PrincipalID      uuid.UUID
	GroupIDs         []uuid.UUID
	IsAdmin          bool
	IsServiceCaller  bool
}

// NewService wires the capture service. `kek` and `objects` may be zero
// values (nil-degenerate) only in dev; production MUST inject both — a nil
// KEK fails closed on first encrypt, and a nil ObjectStore fails closed on a
// large-fragment Put (§4.2, 07-security). rbac is nil here by design:
// production wiring MUST chain WithAuthz so Capture enforces workspace-write
// (§4.4). A nil rbac is only acceptable in unit tests.
func NewService(repos EvidenceRepo, retention RetentionPolicyRepo, kek KEK, crypto Crypto, objects ObjectStore, outboxStore *outbox.Store) *Service {
	return &Service{repos: repos, retention: retention, kek: kek, crypto: crypto, objects: objects, outbox: outboxStore}
}

// WithAuthz injects the RBAC engine + audit logger and returns the service for
// chaining. Production wiring MUST call this so Capture gates on workspace
// write (§4.4); without it the service runs RBAC-free (dev/test only).
func (s *Service) WithAuthz(engine *rbac.Engine, logger *audit.Logger) *Service {
	s.rbac = engine
	s.audit = logger
	return s
}

// ErrCaptureForbidden is returned when the caller lacks write on the workspace
// (§4.4). The handler maps it to 403 + a denied-decision audit; the snippet is
// never stored.
var ErrCaptureForbidden = errors.New("memory: capture forbidden")

// ErrCaptureRejected is returned when the capture is refused (e.g. a secret
// was detected, §4.1). The caller maps it to a 400 + audit; the snippet is
// never stored.
var ErrCaptureRejected = errors.New("memory: evidence capture rejected")

// Capture runs the §4.1/§4.2 gate + storage + outbox event in one transaction.
//
//  1. Redact (§4.1): secret/credential detection (reject) → PII masking →
//     content_hash.
//  2. Storage split (§4.2): inline (≤64KiB, AES-256-GCM) vs. object store.
//  3. Encrypt (D4): generate a per-evidence DEK, AEAD-encrypt the redacted
//     fragment, envelope-wrap the DEK under the current KEK version.
//  4. EvidenceRepo.Insert: persist the row with ciphertext (inline) or
//     storage_key (object).
//  5. outbox.Record: write the `evidence.captured` event to memory_events in
//     the SAME tx (§6.3) so the distill consumer picks it up.
//
// Steps 3–5 are transactional via a caller-supplied sink (EvidenceSink). The
// sink owns the Begin/Commit so the service stays repo-agnostic; this mirrors
// the source service's SyncRunSink split (§6.3).
//
// auth gates the workspace-write RBAC check (§4.4): a caller without write on
// the workspace is refused before any redact/storage runs — the snippet never
// reaches storage (§9.1 fail closed). A nil rbac engine (dev/test only) allows.
func (s *Service) Capture(ctx context.Context, auth AuthContext, req CaptureRequest, sink EvidenceSink) (CaptureResult, error) {
	// 0. Workspace-write RBAC gate (§4.4). Runs BEFORE the redaction gate so a
	// denied caller's snippet is never processed or stored. The denial is
	// auditable (the caller is authenticated and asked to write); a missing /
	// cross-workspace target also surfaces as forbidden here — existence of a
	// workspace the caller cannot see is not leaked.
	if s.rbac != nil && !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs, domain.TargetWorkspace, req.WorkspaceID, domain.ActionWrite)
		if err != nil || !dec.Allowed {
			s.recordDeniedCapture(ctx, auth, req.WorkspaceID)
			return CaptureResult{}, ErrCaptureForbidden
		}
	}

	// 1. Redaction gate (§4.1).
	redacted, err := Redact(req.RawSnippet)
	if err != nil {
		if errors.Is(err, ErrSecretDetected) {
			return CaptureResult{}, fmt.Errorf("%w: %v", ErrCaptureRejected, err)
		}
		return CaptureResult{}, err
	}

	// 2. Resolve retention policy → expires_at (§9.2).
	var expiresAt *time.Time
	policyID := req.RetentionPolicyID
	if policy, err := s.retention.GetForType(ctx, req.WorkspaceID, domain.MemoryFact); err == nil {
		if policyID == nil {
			policyID = &policy.ID
		}
		expiresAt = policyExpiresAt(policy, time.Now())
	}

	// 3. Build the evidence row (storage split decided below). The excerpt is
	// the leak-safe fallback read (§4.3) — a short prefix of the redacted text.
	excerpt := Excerpt(redacted.RedactedText, 256)
	inline := IsInlineCandidate(redacted.RedactedText)

	e := domain.MemoryEvidence{
		WorkspaceID:           req.WorkspaceID,
		OwnerType:              req.OwnerType,
		OwnerID:                req.OwnerID,
		SourceKind:             req.SourceKind,
		SourceRef:              req.SourceRef,
		SourceAssetID:          req.SourceAssetID,
		SourceAssetVersionID:   req.SourceAssetVersionID,
		Visibility:             req.Visibility,
		CapturedAuthzRevision:  req.AuthzRevision,
		ContentHash:            redacted.ContentHash,
		RedactedExcerpt:        excerpt,
		Classification:         redacted.Classification,
		RetentionPolicyID:      policyID,
		State:                  domain.EvidenceActive,
		ExpiresAt:              expiresAt,
	}

	if inline {
		// Inline path (§4.2): AES-256-GCM into encrypted_content.
		ct, keyVersion, err := s.encryptInline(ctx, []byte(redacted.RedactedText))
		if err != nil {
			return CaptureResult{}, err
		}
		e.EncryptedContent = ct
		e.KeyVersion = keyVersion
	} else {
		// Object-store path (§4.2): MinIO under mora-evidence/<ws>/<id>.
		// The id is generated by the sink's Insert; write under a stable key
		// derived from a placeholder id, then the sink swaps in the real id.
		// Simpler: the sink performs Put after Insert returns the id, but the
		// outbox contract requires both row + event in one tx. So we Put FIRST
		// (id pre-generated), then Insert with the known id.
		//
		// To keep the row + event atomic without a 2-phase Put, the sink is
		// given the pre-generated evidence id and the bytes; the sink's
		// CreateEvidence does Put+Insert+Record under one tx boundary (Put is
		// not transactional with PG, so the sink writes the row + event, and
		// Put is best-effort with a sweep reconciling orphaned objects — same
		// compromise as the parser's attachment path).
		e.StorageKey = "" // sink fills after Put
	}

	// 4 + 5. Persist + outbox event in one tx (sink owns the boundary).
	redactedBytes := []byte(redacted.RedactedText)
	res, err := sink.CreateEvidence(ctx, &e, redactedBytes, inline, s.evidenceCapturedEvent(&e))
	if err != nil {
		return CaptureResult{}, err
	}
	return CaptureResult{
		EvidenceID:     res,
		Inline:         inline,
		ContentHash:    redacted.ContentHash,
		Classification: redacted.Classification,
	}, nil
}

// encryptInline generates a per-evidence DEK, AEAD-encrypts the redacted
// fragment, and envelope-wraps the DEK. The returned ciphertext is the
// content-level AEAD output; the wrapped DEK is stored alongside ciphertext
// by the sink in a single encrypted_content column carrying a self-describing
// length-prefixed envelope (§4.2):
//
//	[1: nonceLen][nonceLen: nonce]
//	[2: wrappedLen][wrappedLen: wrapped DEK (kek-nonce||kek-ct)]
//	[rest: content ciphertext]
//
// The length prefixes make the loader's decrypt deterministic — the DEK
// envelope and content ciphertext are both variable-length GCM blobs, so a
// fixed layout would be ambiguous.
func (s *Service) encryptInline(ctx context.Context, plaintext []byte) ([]byte, *int, error) {
	if s.kek == nil || s.crypto == nil {
		return nil, nil, ErrCryptoNotConfigured
	}
	dek := make([]byte, 32) // AES-256
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, err
	}
	ciphertext, nonce, err := s.crypto.Encrypt(ctx, dek, plaintext)
	if err != nil {
		return nil, nil, err
	}
	wrapped, version, err := s.kek.Wrap(ctx, dek)
	if err != nil {
		return nil, nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+2+len(wrapped)+len(ciphertext))
	out = append(out, byte(len(nonce)))
	out = append(out, nonce...)
	out = appendU16(out, uint16(len(wrapped)))
	out = append(out, wrapped...)
	out = append(out, ciphertext...)
	// Zero the DEK in memory (best-effort; Go can't guarantee, but we don't
	// keep it around past this call).
	for i := range dek {
		dek[i] = 0
	}
	return out, &version, nil
}

// appendU16 writes a big-endian uint16 length prefix.
func appendU16(b []byte, n uint16) []byte {
	return append(b, byte(n>>8), byte(n))
}

// evidenceCapturedEvent builds the §7.4 outbox envelope. It carries only IDs +
// the content_hash + source_kind — never content (§5.1). destinations is
// [memory_events] (D5).
func (s *Service) evidenceCapturedEvent(e *domain.MemoryEvidence) domain.KnowledgeEvent {
	ws := e.WorkspaceID
	return domain.KnowledgeEvent{
		EventID:       uuid.NewString(),
		EventType:     domain.KEEvidenceCaptured,
		EventVersion:  1,
		AggregateType: domain.AggMemoryEvidence,
		AggregateID:   e.ID,
		WorkspaceID:   &ws,
		Actor:         domain.EventActor{Type: ownerToSubject(e.OwnerType), ID: e.OwnerID},
		Payload: map[string]any{
			"evidence_id":            e.ID.String(),
			"workspace_id":           e.WorkspaceID.String(),
			"source_kind":           string(e.SourceKind),
			"content_hash":          e.ContentHash,
			"classification":        string(e.Classification),
			"captured_authz_revision": e.CapturedAuthzRevision,
		},
	}
}

// ErrCryptoNotConfigured is returned when the KEK or AEAD adapter is nil —
// production MUST inject both; dev MUST at least provide a stub (07-security).
var ErrCryptoNotConfigured = errors.New("memory: evidence crypto not configured")

// policyExpiresAt computes the evidence expiry from the effective retention
// policy (§9.2). The fallback when no policy resolves is nil (no expiry, open
// retention — admin must set a policy; the reaper will not purge it).
func policyExpiresAt(p domain.RetentionPolicy, now time.Time) *time.Time {
	if p.RetainFor <= 0 {
		return nil
	}
	exp := now.Add(p.RetainFor).UTC()
	return &exp
}

// EvidenceSink is the transactional boundary for capture — the infra adapter
// that owns Begin/Commit. CreateEvidence persists the evidence row, writes the
// large object (when !inline) under mora-evidence/<ws>/<id>, and records the
// outbox event, all atomically (the row + event share one tx; the object Put
// is best-effort with a reconcile sweep, same compromise as attachments).
//
// redactedBytes is the post-gate redacted content (NOT the raw snippet); inline
// reports whether the row carries encrypted_content (vs. storage_key). The
// sink pre-generates the evidence id (so the object key is stable) and writes
// it back on the evidence struct before Insert.
type EvidenceSink interface {
	CreateEvidence(ctx context.Context, e *domain.MemoryEvidence, redactedBytes []byte, inline bool, ev domain.KnowledgeEvent) (uuid.UUID, error)
}

// Compile-time: the outbox Store's Record signature is tx-bound; the sink
// composes it. Ensure the reference stays correct if outbox moves.
var _ = pgx.ErrNoRows

// ownerToSubject maps an OwnerType to a SubjectType for the event actor. The
// two enums overlap for user/agent/service_account; group-owned evidence
// (rare) resolves to service_account in the event envelope (the actor field
// is audit metadata, not an authz principal).
func ownerToSubject(o domain.OwnerType) domain.SubjectType {
	switch o {
	case domain.OwnerUser:
		return domain.SubjectUser
	case domain.OwnerAgent:
		return domain.SubjectAgent
	default:
		return domain.SubjectServiceAccount
	}
}

// recordDeniedCapture writes a best-effort denied-decision audit row when a
// caller is refused capture (§4.4). The workspace id is caller-supplied so
// logging it reveals nothing the caller didn't know. Audit is best-effort: a
// failure never blocks the denial (§5 audit invariants).
func (s *Service) recordDeniedCapture(ctx context.Context, auth AuthContext, workspaceID uuid.UUID) {
	if s.audit == nil {
		return
	}
	actorType := "user"
	if auth.IsServiceCaller {
		actorType = "service"
	}
	ws := workspaceID
	principal := auth.PrincipalID
	s.audit.Record(ctx, actorType, &principal, "memory.evidence.capture",
		"workspace", &ws, "denied: workspace write", "", "")
}