package service

// service_security_test.go covers the §10.2 credential-isolation cases that
// live in the Source application service (design-docs/14 §10.2 用例 16 + 17).
//
// 用例 16 (Run snapshot): when a sync run is enqueued, its
// source_config_snapshot is a REDACTED, FROZEN copy of the source — it carries
// source_type / uri_normalized / sync_policy / trust_level but NEVER
// credential_ref. A later edit to the Source (name, sync_policy, even
// trust_level) must not drift the already-queued run.
//
// 用例 17 (credential rotation): the run pins CredentialVersion at create
// time (§7.2). A credential rotation on the Source after the run is queued
// changes src.CredentialRef, but the run's pinned value is unchanged — the
// in-flight run keeps the credential version it was created with.
//
// 用例 15 (uri strip) is covered in the handler package
// (source_security_test.go::TestStripURICredentials_*). 用例 18 (redaction)
// is covered in the worker package (runner_test.go::TestRedact_*).

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTriggerSync_SnapshotIsRedacted asserts the Run's SourceConfigSnapshot
// carries the fields the Connector needs (source_type, uri_normalized,
// sync_policy, trust_level) but NEVER the credential_ref (§10.2 用例 16,
// §7.2 redacted snapshot). The snapshot is what the Connector eventually
// receives; it must not carry plaintext credential pointers.
func TestTriggerSync_SnapshotIsRedacted(t *testing.T) {
	src := &domain.KnowledgeSource{
		ID:             uuid.New(),
		WorkspaceID:    uuid.New(),
		Enabled:        true,
		SourceType:     domain.SourceGit,
		TrustLevel:     domain.TrustInternal,
		URINormalized:  "https://git.acme/repo.git",
		CredentialRef:  "secret:backend/v3", // the secret pointer — must NOT appear in the snapshot
		SyncPolicy:     map[string]any{"max_bytes": 4096},
	}
	srcs := &fakeSourceRepo{get: src}
	sink := &fakeRunSink{}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, sink, nil)

	_, err := svc.TriggerSync(context.Background(), AuthContext{SubjectType: domain.SubjectUser, PrincipalID: uuid.New()}, TriggerSyncInput{
		SourceID:           src.ID,
		RequestedAssetType: domain.RequestedAssetDocument,
		RequestedByType:   domain.SubjectUser,
		RequestedByID:     uuid.New(),
		IdempotencyKey:    "snap-redact",
	})
	require.NoError(t, err)
	require.NotNil(t, sink.run, "a run must be enqueued")
	snap := sink.run.SourceConfigSnapshot
	require.NotNil(t, snap, "snapshot must be populated")

	// The snapshot carries the fields the Connector needs.
	assert.Equal(t, "git", snap["source_type"])
	assert.Equal(t, "https://git.acme/repo.git", snap["uri_normalized"])
	assert.Equal(t, "internal", snap["trust_level"])

	// The §10.2 用例 16 red line: the snapshot must NOT carry the credential
	// pointer. It is pinned separately on the run as CredentialVersion (用例 17).
	_, hasCredRef := snap["credential_ref"]
	assert.False(t, hasCredRef, "snapshot must not expose credential_ref (§7.2 redacted snapshot)")
	for k := range snap {
		assert.NotContains(t, k, "credential", "snapshot key %q leaks credential semantics", k)
	}
}

// TestTriggerSync_SnapshotIsFrozenAgainstLaterEdit asserts the Run's
// SourceConfigSnapshot is frozen at create time: after the run is enqueued,
// mutating the source's sync_policy / trust_level / name does NOT change the
// snapshot the run already holds (§10.2 用例 16, §7.2 immutability).
func TestTriggerSync_SnapshotIsFrozenAgainstLaterEdit(t *testing.T) {
	src := &domain.KnowledgeSource{
		ID:             uuid.New(),
		WorkspaceID:    uuid.New(),
		Enabled:        true,
		SourceType:     domain.SourceURLAPI,
		TrustLevel:     domain.TrustUntrusted,
		URINormalized:  "https://api.example/feed",
		SyncPolicy:     map[string]any{"max_bytes": 2048},
	}
	srcs := &fakeSourceRepo{get: src}
	sink := &fakeRunSink{}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, sink, nil)

	_, err := svc.TriggerSync(context.Background(), AuthContext{SubjectType: domain.SubjectUser, PrincipalID: uuid.New()}, TriggerSyncInput{
		SourceID:           src.ID,
		RequestedAssetType: domain.RequestedAssetDocument,
		IdempotencyKey:    "snap-freeze",
	})
	require.NoError(t, err)
	require.NotNil(t, sink.run)
	snapAtCreate := sink.run.SourceConfigSnapshot
	assert.Equal(t, "untrusted", snapAtCreate["trust_level"])

	// The source is later edited (trust escalated, sync_policy widened). This
	// is the §10.2 用例 16 scenario: the edit must not drift the queued run.
	src.TrustLevel = domain.TrustTrusted
	src.SyncPolicy = map[string]any{"max_bytes": 999999, "extra": "yes"}

	// The snapshot the run holds is unaffected — snapshotOf copied by value.
	assert.Equal(t, "untrusted", snapAtCreate["trust_level"],
		"a post-enqueue source edit must not drift the run's frozen snapshot")
	assert.Equal(t, 2048, snapAtCreate["sync_policy"].(map[string]any)["max_bytes"],
		"the snapshot's sync_policy is the copy taken at create time")
}

// TestTriggerSync_CredentialVersionPinnedAtCreate asserts the run pins
// CredentialVersion at create time from the source's current CredentialRef
// (§10.2 用例 17 / §7.2). After the run is enqueued, a credential rotation
// changes src.CredentialRef, but the run's pinned CredentialVersion is
// unchanged — the in-flight run keeps the credential version it started with,
// and a new run picks up the new version.
func TestTriggerSync_CredentialVersionPinnedAtCreate(t *testing.T) {
	src := &domain.KnowledgeSource{
		ID:             uuid.New(),
		WorkspaceID:    uuid.New(),
		Enabled:        true,
		SourceType:     domain.SourceGit,
		TrustLevel:     domain.TrustInternal,
		URINormalized:  "https://git.acme/repo.git",
		CredentialRef:  "secret:backend/v1", // current credential version at create time
	}
	srcs := &fakeSourceRepo{get: src}
	sink := &fakeRunSink{}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, sink, nil)

	// Run #1 created while the source points at v1.
	_, err := svc.TriggerSync(context.Background(), AuthContext{SubjectType: domain.SubjectUser, PrincipalID: uuid.New()}, TriggerSyncInput{
		SourceID:           src.ID,
		RequestedAssetType: domain.RequestedAssetDocument,
		RequestedByType:   domain.SubjectUser,
		RequestedByID:     uuid.New(),
		IdempotencyKey:    "rot-1",
	})
	require.NoError(t, err)
	run1 := sink.run
	require.NotNil(t, run1)
	assert.Equal(t, "secret:backend/v1", run1.CredentialVersion,
		"the run pins the source's credential version at create time (§7.2)")

	// Credential rotation: the source's CredentialRef moves to v2. The §10.2
	// 用例 17 invariant: the already-queued run #1 keeps its pinned v1.
	src.CredentialRef = "secret:backend/v2"

	// Run #2 created after the rotation picks up the new v2.
	_, err = svc.TriggerSync(context.Background(), AuthContext{SubjectType: domain.SubjectUser, PrincipalID: uuid.New()}, TriggerSyncInput{
		SourceID:           src.ID,
		RequestedAssetType: domain.RequestedAssetDocument,
		RequestedByType:   domain.SubjectUser,
		RequestedByID:     uuid.New(),
		IdempotencyKey:    "rot-2",
	})
	require.NoError(t, err)
	run2 := sink.run
	require.NotNil(t, run2)
	assert.Equal(t, "secret:backend/v2", run2.CredentialVersion,
		"a new run created after rotation pins the new credential version")

	// The §10.2 用例 17 red line: rotation did not drift run #1's pinned version.
	assert.Equal(t, "secret:backend/v1", run1.CredentialVersion,
		"a credential rotation must not drift an already-queued run's pinned version")
	assert.NotEqual(t, run1.CredentialVersion, run2.CredentialVersion,
		"the two runs must carry distinct pinned versions across the rotation")
}
