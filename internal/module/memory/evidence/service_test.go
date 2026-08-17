package evidence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/outbox"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// fakeSink records what CreateEvidence received without touching PG.
type fakeSink struct {
	gotEvidence *domain.MemoryEvidence
	gotInline   bool
	gotEvent    domain.KnowledgeEvent
	id          uuid.UUID
	err         error
}

func (f *fakeSink) CreateEvidence(ctx context.Context, e *domain.MemoryEvidence, redactedBytes []byte, inline bool, ev domain.KnowledgeEvent) (uuid.UUID, error) {
	if f.err != nil {
		return uuid.Nil, f.err
	}
	f.gotEvidence = e
	f.gotInline = inline
	f.gotEvent = ev
	if f.id == uuid.Nil {
		e.ID = uuid.New()
	} else {
		e.ID = f.id
	}
	return e.ID, nil
}

// fakeRetention returns a 365-day policy for any lookup.
type fakeRetention struct{ d time.Duration }

func (f *fakeRetention) GetForType(ctx context.Context, ws uuid.UUID, mt domain.MemoryType) (domain.RetentionPolicy, error) {
	return domain.RetentionPolicy{ID: uuid.New(), RetainFor: f.d}, nil
}
func (f *fakeRetention) Insert(ctx context.Context, p domain.RetentionPolicy) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (f *fakeRetention) Get(ctx context.Context, id uuid.UUID) (domain.RetentionPolicy, error) {
	return domain.RetentionPolicy{}, nil
}
func (f *fakeRetention) ListForWorkspace(ctx context.Context, ws uuid.UUID) ([]domain.RetentionPolicy, error) {
	return nil, nil
}
func (f *fakeRetention) PurgeDue(ctx context.Context, now time.Time, limit int) ([]domain.MemoryEvidence, error) {
	return nil, nil
}

// fakeKEK + fakeCrypto satisfy the ports for the inline path.
type fakeKEK struct{ version int }

func (k *fakeKEK) Wrap(ctx context.Context, dek []byte) ([]byte, int, error) {
	return append([]byte("wrapped:"), dek...), k.version, nil
}
func (k *fakeKEK) Unwrap(ctx context.Context, wrapped []byte, version int) ([]byte, error) {
	return nil, nil
}
func (k *fakeKEK) CurrentVersion(ctx context.Context) (int, error) { return k.version, nil }

type fakeCrypto struct{}

func (fakeCrypto) Encrypt(ctx context.Context, dek, plaintext []byte) ([]byte, []byte, error) {
	return append([]byte("ct:"), plaintext...), []byte("nonce"), nil
}
func (fakeCrypto) Decrypt(ctx context.Context, dek, ciphertext, nonce []byte) ([]byte, error) {
	return nil, nil
}

// compile-time: the fakes satisfy the ports.
var (
	_ EvidenceSink       = (*fakeSink)(nil)
	_ RetentionPolicyRepo = (*fakeRetention)(nil)
	_ KEK                = (*fakeKEK)(nil)
	_ Crypto             = fakeCrypto{}
)

func TestService_Capture_Inline(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	svc := NewService(nil, &fakeRetention{d: 365 * 24 * time.Hour}, &fakeKEK{version: 1}, fakeCrypto{}, nil, outbox.NewStore())
	req := CaptureRequest{
		WorkspaceID:     uuid.New(),
		OwnerType:       domain.OwnerAgent,
		OwnerID:         uuid.New(),
		SourceKind:      domain.EvidenceSourceToolCall,
		SourceRef:       "tool_call:abc",
		Visibility:      domain.EvidencePrivate,
		RawSnippet:      "决策：mora-api 监听 :8990，工作区隔离走 ltree path。",
		AuthzRevision:   42,
	}
	res, err := svc.Capture(context.Background(), AuthContext{}, req, sink)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !res.Inline {
		t.Fatal("small fragment should be inline")
	}
	if res.ContentHash == "" {
		t.Fatal("content hash empty")
	}
	if sink.gotEvent.EventType != domain.KEEvidenceCaptured {
		t.Fatalf("event type %q != evidence.captured", sink.gotEvent.EventType)
	}
	if sink.gotEvidence.EncryptedContent == nil {
		t.Fatal("encrypted_content nil for inline path")
	}
	if sink.gotEvidence.KeyVersion == nil || *sink.gotEvidence.KeyVersion != 1 {
		t.Fatalf("key_version wrong: %v", sink.gotEvidence.KeyVersion)
	}
	if sink.gotEvidence.ExpiresAt == nil {
		t.Fatal("expires_at should be set from retention policy")
	}
	if sink.gotEvidence.State != domain.EvidenceActive {
		t.Fatalf("state = %q, want active", sink.gotEvidence.State)
	}
	if sink.gotEvidence.RedactedExcerpt == "" {
		t.Fatal("redacted_excerpt empty")
	}
}

func TestService_Capture_RejectsSecret(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	svc := NewService(nil, &fakeRetention{d: 24 * time.Hour}, &fakeKEK{version: 1}, fakeCrypto{}, nil, outbox.NewStore())
	req := CaptureRequest{
		WorkspaceID: uuid.New(),
		OwnerType:    domain.OwnerUser,
		OwnerID:      uuid.New(),
		SourceKind:   domain.EvidenceSourceSession,
		SourceRef:    "session:s1",
		Visibility:   domain.EvidencePrivate,
		RawSnippet:   `the api_key = "sk-1234567890abcdefghijklmnop1234567890" oops`,
		AuthzRevision: 1,
	}
	_, err := svc.Capture(context.Background(), AuthContext{}, req, sink)
	if err == nil {
		t.Fatal("expected ErrCaptureRejected for secret")
	}
	if !errors.Is(err, ErrCaptureRejected) {
		t.Fatalf("expected ErrCaptureRejected, got %v", err)
	}
	if sink.gotEvidence != nil {
		t.Fatal("sink must not be called when a secret is detected")
	}
}

func TestService_Capture_MasksPII(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	svc := NewService(nil, &fakeRetention{d: 24 * time.Hour}, &fakeKEK{version: 1}, fakeCrypto{}, nil, outbox.NewStore())
	req := CaptureRequest{
		WorkspaceID: uuid.New(),
		OwnerType:    domain.OwnerUser,
		OwnerID:      uuid.New(),
		SourceKind:   domain.EvidenceSourceMessage,
		SourceRef:    "msg:m1",
		Visibility:   domain.EvidencePrivate,
		RawSnippet:   "联系 alice@example.com 获取 runbook",
		AuthzRevision: 1,
	}
	res, err := svc.Capture(context.Background(), AuthContext{}, req, sink)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if res.Classification != domain.EvidenceClassPII {
		t.Fatalf("expected pii classification, got %q", res.Classification)
	}
	// The excerpt must not contain the raw email.
	if contains(sink.gotEvidence.RedactedExcerpt, "alice@example.com") {
		t.Fatal("redacted_excerpt leaked PII")
	}
}

func TestService_Capture_ObjectStorePathNotInline(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	svc := NewService(nil, &fakeRetention{d: 24 * time.Hour}, &fakeKEK{version: 1}, fakeCrypto{}, nil, outbox.NewStore())
	// A fragment above the inline ceiling would route to object storage; but
	// without a real ObjectStore the service still builds the row and lets the
	// sink Put. Here the sink is fake (no Put), so only verify the split
	// decision: a huge fragment is NOT inline.
	big := make([]byte, domain.EvidenceInlineMaxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	req := CaptureRequest{
		WorkspaceID:  uuid.New(),
		OwnerType:     domain.OwnerAgent,
		OwnerID:       uuid.New(),
		SourceKind:    domain.EvidenceSourceCode,
		SourceRef:     "code:c1",
		Visibility:    domain.EvidencePrivate,
		RawSnippet:    string(big),
		AuthzRevision: 1,
	}
	res, err := svc.Capture(context.Background(), AuthContext{}, req, sink)
	if err != nil {
		t.Fatalf("Capture (object path): %v", err)
	}
	if res.Inline {
		t.Fatal("large fragment should NOT be inline")
	}
}

// denyRBACRepo is a no-grants Repository so rbac.Engine.Check denies every
// write (§4.4: default deny). It satisfies rbac.Repository; the unused methods
// return zero values because the evidence gate only exercises GrantsFor.
type denyRBACRepo struct{}

func (denyRBACRepo) GrantsFor(ctx context.Context, subjectID uuid.UUID, groupIDs []uuid.UUID, workspaceID uuid.UUID) ([]domain.Grant, error) {
	return nil, nil // no grants → default deny
}
func (denyRBACRepo) DirectoryAncestors(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (denyRBACRepo) DocumentLocation(ctx context.Context, documentID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	return uuid.Nil, uuid.Nil, nil
}
func (denyRBACRepo) DocumentsInDirectorySubtree(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// TestService_Capture_DeniedByRBAC verifies the §4.4 workspace-write gate runs
// BEFORE the redact/encrypt/storage pipeline: a denied caller's snippet never
// reaches the sink (no half-stored evidence, §9.1 fail closed).
func TestService_Capture_DeniedByRBAC(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	svc := NewService(nil, &fakeRetention{d: 24 * time.Hour}, &fakeKEK{version: 1}, fakeCrypto{}, nil, outbox.NewStore()).
		WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil)
	req := CaptureRequest{
		WorkspaceID:  uuid.New(),
		OwnerType:     domain.OwnerUser,
		OwnerID:       uuid.New(),
		SourceKind:    domain.EvidenceSourceSession,
		SourceRef:     "session:s1",
		Visibility:    domain.EvidencePrivate,
		RawSnippet:    "决策：不应被存储，因为调用者无写权限。",
		AuthzRevision: 1,
	}
	_, err := svc.Capture(context.Background(), AuthContext{}, req, sink)
	if !errors.Is(err, ErrCaptureForbidden) {
		t.Fatalf("expected ErrCaptureForbidden, got %v", err)
	}
	if sink.gotEvidence != nil {
		t.Fatal("denied capture must not reach the sink (no half-stored evidence)")
	}
}

// TestService_Capture_AdminBypassesRBAC verifies the IsAdmin short-circuit:
// an admin caller captures even under a deny-all engine (mirrors the
// document/source service pattern).
func TestService_Capture_AdminBypassesRBAC(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	svc := NewService(nil, &fakeRetention{d: 24 * time.Hour}, &fakeKEK{version: 1}, fakeCrypto{}, nil, outbox.NewStore()).
		WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil)
	req := CaptureRequest{
		WorkspaceID:  uuid.New(),
		OwnerType:     domain.OwnerUser,
		OwnerID:       uuid.New(),
		SourceKind:    domain.EvidenceSourceSession,
		SourceRef:     "session:admin",
		Visibility:    domain.EvidencePrivate,
		RawSnippet:    "决策：admin 旁路 RBAC。",
		AuthzRevision: 1,
	}
	res, err := svc.Capture(context.Background(), AuthContext{IsAdmin: true}, req, sink)
	if err != nil {
		t.Fatalf("admin capture: %v", err)
	}
	if res.EvidenceID == uuid.Nil {
		t.Fatal("admin capture should succeed (evidence_id set)")
	}
}

// contains is a tiny helper to avoid pulling strings just for a substring
// check in the test.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
