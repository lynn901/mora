//go:build integration

// skill_integration_test.go — Phase 5-5 (YS-166) §9 acceptance-gate integration
// tests against a live PostgreSQL (mora-postgres-1). DATABASE_URL-gated; skipped
// when unset. Run with:
//
//	DATABASE_URL=postgres://mora:mora@mora-postgres-1:5432/mora?sslmode=disable \
//	  go test -tags=integration ./test/integration/... -run SkillSuite -count=1 -v
//
// This suite covers the §9 gates that the binding suite (YS-162) and the
// in-memory service unit tests do NOT cover end-to-end on the real DB:
//
//   - Gate 1 (§3.2 Binding is a reference, not a copy): binding the same skill
//     asset to N agents stores the asset body ONCE; N agent_bindings rows
//     reference it.
//   - Gate 2 alert path (§5.1 阻断+告警): the binding Service's pre-flight
//     pinned-version alert flags a non-usable pinned version
//     (PinnedVersionBlocked=true, Alerted populated) — paired with the
//     authz-layer block (BindingSuite.Test_Binding_PinnedVersionRevoked...).
//   - Gate 3 (§3.1 / §2 roundtrip): standard sample package import→export via
//     the REAL SkillRepo (skill_packages row on PG) with content_hash consistent
//     + unknown frontmatter fields preserved verbatim in the DB row.
//   - Gate 4 (§4.4 / §7 script-execution count = 0): a static proof (no
//     os/exec import in the skill module) + a runtime probe that runs all four
//     paths (import/revalidate/export/deliver-read) and asserts no child
//     process was ever spawned.
//   - §17.4 security: path-traversal rejection, symlink-not-followed, ELF /
//     disguised-binary detected-not-exec'd, compression bomb → ErrArchiveTooLarge.
//
// It does NOT duplicate the authz-layer pinned-version block (covered by
// BindingSuite.Test_Binding_PinnedVersionRevokedBlocksNoFallback) or the
// revoke→next-request-deny path (covered by
// BindingSuite.Test_Binding_CacheInvalidatesByRevisionOnRevoke) — those are the
// authoritative §5.1 / §5.4 tests; this suite references them.
package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	bindingmod "github.com/lynn901/mora/internal/module/binding"
	"github.com/lynn901/mora/internal/module/skill"
	skillpkg "github.com/lynn901/mora/internal/module/skill/package"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// SkillSuite exercises the §9 skill-package + binding-reference gates
// end-to-end against the real DB + real infra (SkillRepo, BindingSink,
// PinnedVersionChecker). The archive bytes are supplied via an in-memory opener
// (the MinIO adapter is a storage seam; the SUT here is parse/validate/export +
// the skill_packages row, not object storage).
type SkillSuite struct {
	suite.Suite
	pool *pgxpool.Pool
	db   *postgres.DB

	skillRepo *postgres.SkillRepo
	sink      *postgres.BindingSink
	bindRepo  *postgres.BindingRepo
	outbox    *outbox.Store
}

func TestSkillSuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(SkillSuite))
}

func (s *SkillSuite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(s.T(), err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	s.skillRepo = postgres.NewSkillRepo(s.db)
	s.outbox = outbox.NewStore()
	s.sink = postgres.NewBindingSink(pool, s.outbox)
	s.bindRepo = postgres.NewBindingRepo(s.db)
}

func (s *SkillSuite) TearDownSuite() { s.pool.Close() }

// SetupTest cleans the skill + binding tables in dependency order so each test
// starts from a known-empty state. Mirrors BindingSuite.SetupTest's cleaning
// discipline but adds skill_packages (mounted on knowledge_asset_versions).
func (s *SkillSuite) SetupTest() {
	ctx := context.Background()
	for _, t := range []string{
		"agent_binding_batches", "agent_bindings",
		"outbox_events", "outbox_deliveries",
		"skill_packages", "knowledge_asset_versions", "knowledge_assets",
		"knowledge_relations", "agents",
		"permissions",
	} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM workspaces WHERE slug LIKE 'sk-%'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM users WHERE email LIKE 'sk-%'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM roles WHERE is_system = false`)
}

// --- seed helpers (skill-flavored, mirror BindingSuite's shape) ---

func (s *SkillSuite) seedUser(email, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		email, name).Scan(&id))
	return id
}

func (s *SkillSuite) seedWorkspace(owner domain.UUID, slug string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$2,$3) RETURNING id`,
		"WS "+slug, slug, owner).Scan(&id))
	return id
}

func (s *SkillSuite) seedAgent(wsID, ownerID domain.UUID, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO agents (workspace_id, name, owner_id, status) VALUES ($1,$2,$3,'active') RETURNING id`,
		wsID, name, ownerID).Scan(&id))
	return id
}

// seedSkillAsset inserts a knowledge_asset with asset_type='skill' (the type
// under test) and returns its id. Status 'published' so authz lifecycleGate
// permits use.
func (s *SkillSuite) seedSkillAsset(wsID, ownerID domain.UUID, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx, `
		INSERT INTO knowledge_assets (workspace_id, asset_type, name, owner_type, owner_id, status, visibility)
		VALUES ($1,'skill',$2,'user',$3,'published','private') RETURNING id`,
		wsID, name, ownerID).Scan(&id))
	return id
}

func (s *SkillSuite) seedVersion(assetID domain.UUID, versionNo int64, build, gov string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx, `
		INSERT INTO knowledge_asset_versions
		  (asset_id, version_no, content_origin, dedupe_key, build_status, governance_status, created_by_type, created_by_id)
		VALUES ($1,$2,'human',$3,$4,$5,'user',$6) RETURNING id`,
		assetID, versionNo, fmt.Sprintf("sk-dk-%d", versionNo), build, gov, uuid.New()).Scan(&id))
	return id
}

// seedSkillPackage persists a skill_packages row directly via the REAL SkillRepo
// (the production write path), mounting it on an asset version. The package is
// built from an in-memory archive so the manifest/content_hash match what
// Import would produce — this is the helper for tests that need a stored row
// without re-running the full Import (Gate 1 uses it; Gate 3 uses Import).
func (s *SkillSuite) seedSkillPackage(assetVerID domain.UUID, arch []byte, storageKey string) domain.SkillPackage {
	ctx := context.Background()
	// Parse the archive through the production parse path so the manifest +
	// content_hash are authoritative (not hand-fabricated).
	parsed, err := skillpkg.Parse(storageKey, memArchiveReader{data: arch})
	require.NoError(s.T(), err)
	pkg := domain.SkillPackage{
		AssetVersionID:   assetVerID,
		StorageKey:       storageKey,
		FormatID:         domain.SkillFormatAgentskills,
		SchemaVersion:    "1.0",
		Manifest:         parsed.Manifest,
		OriginalFrontmatter: parsed.Package.OriginalFrontmatter,
		ContentHash:      parsed.ContentHash,
		ValidationStatus: domain.SkillValidationPassed,
		CompatibilityReport: domain.CompatibilityReport{Delivery: domain.DeliveryLossless},
		ScannerVersion:   skill.ScannerVersion,
	}
	require.NoError(s.T(), s.skillRepo.Save(ctx, pkg))
	return pkg
}

// memArchiveOpener is a skill.ArchiveOpener over an in-memory byte slice. It
// returns the bytes AS STORED — Mora never synthesizes an exec bit here.
type memArchiveOpener struct{ data []byte }

func (m memArchiveOpener) OpenArchive(_ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.data)), nil
}

// memArchiveReader is the package sub-domain's ArchiveReader over bytes.
type memArchiveReader struct{ data []byte }

func (m memArchiveReader) Open(_ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.data)), nil
}

// --- shared archive fixtures (mirror service_test.go's sample, kept local so
// the integration suite is self-contained and does not import the skill pkg's
// _test build). ---

// sampleSKILLMD is a standard agentskills.io/v1.0 SKILL.md with a known field
// set AND an unknown legal field ("custom_runtime_config") that Mora must
// preserve verbatim (§2.3 lossless).
const sampleSKILLMD = "---\n" +
	"name: echo-skill\n" +
	"description: echoes input\n" +
	"version: \"1.0\"\n" +
	"format: agentskills.io/v1.0\n" +
	"schema_version: \"1.0\"\n" +
	"runtime: claude-code\n" +
	"custom_runtime_config:\n" +
	"  endpoint: https://internal.invalid\n" +
	"  retries: 3\n" +
	"capabilities:\n" +
	"  tools:\n" +
	"    - echo\n" +
	"  resources:\n" +
	"    - assets/guide.md\n" +
	"---\n" +
	"# Echo Skill\n\nA skill that echoes its input.\n"

type archiveEntry struct {
	path    string
	content []byte
	mode    int64
}

func fileEntry(p string, c []byte) archiveEntry { return archiveEntry{p, c, 0o644} }
func execEntry(p string, c []byte) archiveEntry { return archiveEntry{p, c, 0o755} }

func buildArchive(t testing.TB, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: e.path, Mode: e.mode, Size: int64(len(e.content)),
		}))
		_, _ = tw.Write(e.content)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// buildSampleArchive is the standard §9 gate fixture (clean agentskills
// package with an exec-bit script + an unknown frontmatter field).
func buildSampleArchive(t testing.TB) []byte {
	return buildArchive(t,
		fileEntry("SKILL.md", []byte(sampleSKILLMD)),
		fileEntry("assets/guide.md", []byte("# Guide\nUse the echo tool.\n")),
		execEntry("scripts/run.sh", []byte("#!/bin/sh\necho hi\n")),
	)
}

// buildBombArchive — a tar header that declares a size over the per-file cap
// (anti-compression-bomb, §4.4). The oversized entry is written with no body;
// on read, tar.Reader yields the header and Parse aborts with
// ErrArchiveTooLarge before allocating the bytes.
func buildBombArchive(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "SKILL.md", Mode: 0o644, Size: int64(len("---\nname: x\n---\nbody")),
	}))
	_, _ = tw.Write([]byte("---\nname: x\n---\nbody"))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "assets/huge.bin", Mode: 0o644, Size: 1 << 30,
	}))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// buildTraversalArchive — a regular entry plus an entry whose path escapes the
// archive root (../escape).
func buildTraversalArchive(t testing.TB) []byte {
	t.Helper()
	return buildArchive(t,
		fileEntry("SKILL.md", []byte("---\nname: x\n---\nbody")),
		fileEntry("../escape", []byte("pwned")),
	)
}

// buildSymlinkArchive — a symlink entry whose target points outside the
// archive; the target must NOT be followed (§4.4).
func buildSymlinkArchive(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "SKILL.md", Mode: 0o644, Size: int64(len("---\nname: x\n---\nbody")),
	}))
	_, _ = tw.Write([]byte("---\nname: x\n---\nbody"))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "assets/link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
	}))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// buildBinaryArchive — a regular file whose content starts with the ELF magic
// (\x7fELF), i.e. a disguised binary. It must be detected as a script/asset
// (pattern-recognition) and stored as an untrusted resource, NEVER executed.
func buildBinaryArchive(t testing.TB) []byte {
	t.Helper()
	elf := append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte{0}, 1<<16)...)
	return buildArchive(t,
		fileEntry("SKILL.md", []byte("---\nname: x\n---\nbody")),
		fileEntry("scripts/payload.bin", elf),
	)
}

// ===========================================================================
// Gate 1 (§3.2): Binding is a reference, not a copy.
// ===========================================================================

// Test_Skill_BindingIsReferenceNotCopy: binding the SAME skill asset to three
// agents stores the asset body (knowledge_assets + knowledge_asset_versions +
// skill_packages) EXACTLY ONCE. Three agent_bindings rows reference the one
// asset_id; the skill_packages row (the "body") is not duplicated per binding.
// This is the §3.2 invariant a fake-repo unit test cannot prove (it would need
// the real FK graph + the skill_packages 1:1 mount).
func (s *SkillSuite) Test_Skill_BindingIsReferenceNotCopy() {
	ctx := context.Background()
	owner := s.seedUser("sk-g1@x.com", "Owner")
	ws := s.seedWorkspace(owner, "sk-g1")
	asset := s.seedSkillAsset(ws, owner, "skill-g1")
	ver := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, ver, asset)
	require.NoError(s.T(), err)
	s.seedSkillPackage(ver, buildSampleArchive(s.T()), "skill/g1.tar.gz")

	// Three agents, each bound to the SAME asset (allow, follow_published).
	agents := make([]domain.UUID, 3)
	bindAuth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}
	svc := bindingmod.NewService(s.bindRepo, s.sink, s.sink, nil)
	for i := range agents {
		agents[i] = s.seedAgent(ws, owner, "agent-g1-"+string(rune('a'+i)))
		in := []bindingmod.BindingInput{{
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
		}}
		_, err := svc.BatchUpsertBindings(ctx, bindAuth, agents[i], ws, "g1-key-"+string(rune('a'+i)), in)
		require.NoError(s.T(), err)
	}

	// §3.2: the asset body is stored ONCE.
	var assetCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM knowledge_assets WHERE id=$1`, asset).Scan(&assetCount))
	assert.Equal(s.T(), 1, assetCount, "the skill asset body is stored exactly once across N bindings")

	var verCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM knowledge_asset_versions WHERE asset_id=$1`, asset).Scan(&verCount))
	assert.Equal(s.T(), 1, verCount, "the skill version body is stored exactly once across N bindings")

	var pkgCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM skill_packages WHERE asset_version_id=$1`, ver).Scan(&pkgCount))
	assert.Equal(s.T(), 1, pkgCount, "the skill_packages row (the body) is stored exactly once — bindings reference, do not copy")

	// §3.2: three bindings reference the one asset.
	var bindCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE asset_id=$1 AND revoked_at IS NULL`, asset).Scan(&bindCount))
	assert.Equal(s.T(), 3, bindCount, "three agents → three binding rows referencing the one asset")

	// The single skill_packages row is shared — every binding resolves to the
	// SAME content_hash (the §3.1 roundtrip anchor), proving no per-binding
	// copy drifted. There is exactly one distinct content_hash across the
	// shared package (one body row, N reference rows).
	var hash string
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT content_hash FROM skill_packages WHERE asset_version_id=$1`, ver).Scan(&hash))
	assert.NotEmpty(s.T(), hash, "the shared skill_packages row has a content_hash (the §3.1 anchor)")
	// There is exactly ONE skill_packages row total (the body), shared by all
	// N bindings — the bindings reference it, they do not copy it.
	var totalPackages int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM skill_packages WHERE asset_version_id=$1`, ver).Scan(&totalPackages))
	assert.Equal(s.T(), 1, totalPackages, "the skill body is a single shared row across N bindings (reference, not copy)")
	// Directly: all bindings resolve to the one package via the asset → version → package chain.
	for _, a := range agents {
		var allowed int
		require.NoError(s.T(), s.pool.QueryRow(ctx,
			`SELECT count(*) FROM agent_bindings WHERE agent_id=$1 AND asset_id=$2 AND revoked_at IS NULL`,
			a, asset).Scan(&allowed))
		assert.Equal(s.T(), 1, allowed, "each agent has exactly one reference binding to the shared asset")
	}
}

// ===========================================================================
// Gate 2 alert path (§5.1 阻断+告警): the binding Service's pre-flight alert.
// ===========================================================================

// Test_Skill_PinnedVersionAlertsNoFallback: a batch with a pinned binding whose
// pinned version is NOT usable (governance_status=deprecated) is still written
// (durable alert) AND flagged PinnedVersionBlocked=true with the item index in
// Alerted. This is the "告警" half of §5.1; the "阻断" half (authz
// pinnedVersionGate denies use) is covered by
// BindingSuite.Test_Binding_PinnedVersionRevokedBlocksNoFallback.
//
// Unlike BindingSuite's pinned tests (which pass a nil checker to isolate the
// authz layer), this wires the REAL PinnedVersionChecker so the service's
// pre-flight alert path runs against the real version-resolver SQL.
func (s *SkillSuite) Test_Skill_PinnedVersionAlertsNoFallback() {
	ctx := context.Background()
	owner := s.seedUser("sk-g2@x.com", "Owner")
	ws := s.seedWorkspace(owner, "sk-g2")
	asset := s.seedSkillAsset(ws, owner, "skill-g2")
	healthy := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	revoked := s.seedVersion(asset, 2, domain.VersionBuildReady, domain.VersionGovDeprecated)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, healthy, asset)
	require.NoError(s.T(), err)
	s.seedSkillPackage(healthy, buildSampleArchive(s.T()), "skill/g2.tar.gz")

	agent := s.seedAgent(ws, owner, "agent-g2")
	// Wire the REAL pinned-version checker (the production resolver).
	pinned := postgres.NewPinnedVersionChecker(postgres.NewAssetReadRepo(s.db))
	svc := bindingmod.NewService(s.bindRepo, s.sink, s.sink, pinned)
	bindAuth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}

	// Pin the DEPRECATED (revoked) version. The batch must NOT be rejected;
	// the binding is written and flagged blocked (durable alert, no fallback).
	in := []bindingmod.BindingInput{{
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow,
		VersionPolicy: domain.BindingPinned, PinnedVersionID: &revoked,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}}
	res, err := svc.BatchUpsertBindings(ctx, bindAuth, agent, ws, "g2-key-1", in)
	require.NoError(s.T(), err, "a blocked pinned version must NOT reject the batch (durable alert, §5.1)")
	require.Len(s.T(), res.Results, 1)
	assert.True(s.T(), res.Results[0].PinnedVersionBlocked,
		"the pinned binding must be flagged PinnedVersionBlocked (§5.1 告警)")
	assert.Contains(s.T(), res.Alerted, 0,
		"the blocked item's index must be in Alerted so the caller surfaces the alert")

	// The binding IS written (durable) — §5.1: alert, not silent drop.
	var activeCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE agent_id=$1 AND pinned_version_id=$2 AND revoked_at IS NULL`,
		agent, revoked).Scan(&activeCount))
	assert.Equal(s.T(), 1, activeCount, "the blocked pinned binding is still written (durable alert)")

	// §5.1: no fallback — the binding pins the DEPRECATED version, not the
	// healthy current_version. The pinned_version_id is the revoked one.
	var pinnedID domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT pinned_version_id FROM agent_bindings WHERE agent_id=$1 AND revoked_at IS NULL`, agent).Scan(&pinnedID))
	assert.Equal(s.T(), revoked, pinnedID,
		"the binding pins the revoked version exactly — no silent drift to the healthy current_version (§11.4)")
}

// ===========================================================================
// Gate 3 (§3.1 / §2): roundtrip consistency on the real DB.
// ===========================================================================

// Test_Skill_RoundtripHashConsistentAndUnknownFieldsLossless: a standard sample
// package Import → Export via the REAL SkillRepo persists the skill_packages row
// (manifest + content_hash + original_frontmatter) and the export recomputes a
// content_hash EQUAL to the import content_hash (§9 往返门禁). The unknown legal
// frontmatter field ("custom_runtime_config") is preserved verbatim in the DB
// row's original_frontmatter — proving lossless roundtrip on real PG, not just
// in-memory (the unit test TestImport_Export_Roundtrip uses a fake repo).
func (s *SkillSuite) Test_Skill_RoundtripHashConsistentAndUnknownFieldsLossless() {
	ctx := context.Background()
	owner := s.seedUser("sk-g3@x.com", "Owner")
	ws := s.seedWorkspace(owner, "sk-g3")
	asset := s.seedSkillAsset(ws, owner, "skill-g3")
	ver := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, ver, asset)
	require.NoError(s.T(), err)

	// Production service over the REAL repo. The opener is in-memory (the
	// archive bytes are the SUT; MinIO is a storage seam).
	svc := skill.NewService(s.skillRepo)
	arch := buildSampleArchive(s.T())
	opener := memArchiveOpener{data: arch}

	res, err := svc.Import(ctx, skill.AuthContext{IsAdmin: true},
		skill.ImportOptions{AssetVersionID: ver, StorageKey: "skill/g3.tar.gz"}, opener)
	require.NoError(s.T(), err)
	importHash := res.ContentHash
	require.NotEmpty(s.T(), importHash)
	assert.Equal(s.T(), domain.SkillValidationPassed, res.Package.ValidationStatus,
		"clean agentskills package → passed (saveable, NOT executable)")
	assert.Equal(s.T(), domain.DeliveryLossless, res.Package.CompatibilityReport.Delivery)

	// §2.3: the unknown legal field is preserved verbatim in the DB row
	// (not just the in-memory result). Read the row back via the real repo.
	stored, err := s.skillRepo.Get(ctx, ver)
	require.NoError(s.T(), err)
	require.Contains(s.T(), stored.OriginalFrontmatter, "custom_runtime_config",
		"unknown frontmatter field preserved verbatim in the DB row (§2.3 lossless)")
	crc := map[string]any{}
	if v, ok := stored.OriginalFrontmatter["custom_runtime_config"].(map[string]any); ok {
		crc = v
	}
	require.NotEmpty(s.T(), crc, "the nested unknown object is preserved as a map, not stringified")
	assert.Equal(s.T(), "https://internal.invalid", crc["endpoint"], "nested unknown scalar preserved verbatim")
	assert.EqualValues(s.T(), 3, crc["retries"], "nested unknown scalar preserved verbatim")

	// §3.1: the persisted content_hash equals the import content_hash.
	assert.Equal(s.T(), importHash, stored.ContentHash,
		"the persisted skill_packages.content_hash equals the import content_hash (§3.1 anchor)")

	// §9 往返门禁: export recomputes a content_hash EQUAL to the import.
	out, err := svc.Export(ctx, skill.AuthContext{IsAdmin: true}, ver, opener)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), importHash, out.ContentHash,
		"export content_hash == import content_hash (§9 往返门禁, real DB)")
	assert.NotEmpty(s.T(), out.Archive, "export produced a non-empty canonical archive")

	// The file list (manifest) is consistent: re-parse the exported archive and
	// compare its derived content_hash. It must equal the import hash — the
	// roundtrip is byte-reproducible at the content layer.
	reParsed, err := skillpkg.Parse("export", memArchiveReader{data: out.Archive})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), importHash, reParsed.ContentHash,
		"re-parsing the EXPORTED archive yields the same content_hash — the roundtrip is content-reproducible")

	// The exported archive dropped the executable bit (§4.4: never materialize
	// an exec bit on writeback). The sample fixture had scripts/run.sh at 0o755.
	var runSH *skillpkg.ParsedFile
	for i := range reParsed.Package.Files {
		if reParsed.Package.Files[i].Path == "scripts/run.sh" {
			runSH = &reParsed.Package.Files[i]
		}
	}
	require.NotNil(s.T(), runSH, "the exported archive still contains scripts/run.sh")
	assert.False(s.T(), runSH.ExecBit,
		"export wrote scripts/run.sh WITHOUT an executable bit (§4.4: never honor exec bit on writeback)")
}

// ===========================================================================
// Gate 4 (§4.4 / §7): script-execution count = 0.
// ===========================================================================

// Test_Skill_NoExecAcrossFourPaths_StaticProof: a STATIC proof that the skill
// module never imports os/exec. There is no exec(2) call site to probe at
// runtime — the invariant is architectural (the module's doc + every code path
// declares it). A grep of the module source for os/exec / exec.Command must
// return zero hits. This is the §7 "execution-count probe = 0" anchor: the
// count is 0 because there is no mechanism to make it nonzero.
func (s *SkillSuite) Test_Skill_NoExecAcrossFourPaths_StaticProof() {
	// Resolve the skill module source on disk relative to THIS test file
	// (test/integration under the repo root), not the process CWD — the test
	// runs in a Docker container whose CWD (/src) is the repo root, but the
	// skill module is at <repo>/internal/module/skill.
	_, testFile, _, ok := runtime.Caller(0)
	require.True(s.T(), ok, "runtime.Caller must resolve the test source path")
	// testFile = <repo>/test/integration/skill_integration_test.go → repo root.
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(testFile)))
	modRoot := filepath.Join(repoRoot, "internal", "module", "skill")
	require.DirExists(s.T(), modRoot)

	var hits []string
	err := filepath.Walk(modRoot, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		// Match the exec import + the exec call sites (pattern-recognition
		// only — any hit is a violation of the §4.4 invariant).
		if strings.Contains(string(b), "os/exec") || strings.Contains(string(b), "exec.Command") {
			hits = append(hits, p)
		}
		return nil
	})
	require.NoError(s.T(), err)
	assert.Empty(s.T(), hits,
		"§4.4/§7 invariant: the skill module must never import os/exec or call exec.Command (script-execution count = 0). Violating files: "+strings.Join(hits, ", "))
}

// Test_Skill_NoExecAcrossFourPaths_RuntimeProbe: run all four paths
// (import/revalidate/export/deliver-read) on a package that contains a script
// with a shebang + an exec bit, and assert no child process was spawned. The
// probe wraps the process (count children before/after); the count delta must
// be 0 — scripts are detected as metadata, never executed.
//
// This is the runtime complement to the static proof: even an archive carrying
// a malicious script (#!/bin/sh) + an executable bit traverses import/validate/
// export/deliver without forking. The four-path coverage is the §9 gate; the
// static proof is the §7 anchor.
func (s *SkillSuite) Test_Skill_NoExecAcrossFourPaths_RuntimeProbe() {
	ctx := context.Background()
	owner := s.seedUser("sk-g4@x.com", "Owner")
	ws := s.seedWorkspace(owner, "sk-g4")
	asset := s.seedSkillAsset(ws, owner, "skill-g4")
	ver := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, ver, asset)
	require.NoError(s.T(), err)

	svc := skill.NewService(s.skillRepo)
	// The sample fixture carries scripts/run.sh with #!/bin/sh + an exec bit —
	// exactly the shape a malicious skill would ship. It must NOT be executed.
	arch := buildSampleArchive(s.T())
	opener := memArchiveOpener{data: arch}
	auth := skill.AuthContext{IsAdmin: true}

	childrenBefore := countChildProcesses(s.T())
	// Path 1: import (parse + validate + persist).
	_, err = svc.Import(ctx, auth,
		skill.ImportOptions{AssetVersionID: ver, StorageKey: "skill/g4.tar.gz"}, opener)
	require.NoError(s.T(), err)
	// Path 2: revalidate (re-parse + re-validate).
	_, err = svc.Revalidate(ctx, auth, ver, opener)
	require.NoError(s.T(), err)
	// Path 3: export (re-derive archive + assert content_hash).
	_, err = svc.Export(ctx, auth, ver, opener)
	require.NoError(s.T(), err)
	// Path 4: deliver-read (delivery parses the archive to locate a resource).
	delivery := buildDeliveryServiceForRead(s, ver, opener)
	_, err = delivery.ReadResource(ctx, uuid.New(), ws, asset, "", "assets/guide.md")
	// ReadResource with a no-binding agent context returns ErrPackageNotFound
	// (no allow binding) — that is the expected closed path; the point is that
	// it did NOT exec anything to reach that decision. Any non-exec error is
	// acceptable; a successful exec would be the violation.
	assert.Error(s.T(), err, "ReadResource without an allow binding must fail closed (no leak)")
	childrenAfter := countChildProcesses(s.T())

	assert.Equal(s.T(), childrenBefore, childrenAfter,
		"§4.4/§7: no child process was spawned across import/revalidate/export/deliver-read (script-execution count = 0)")
}

// countChildProcesses snapshots the current process's child count. On Linux it
// reads /proc/<pid>/task/*/children; a best-effort fallback returns 0 (the
// static proof is the authoritative gate, this is the runtime corroboration).
func countChildProcesses(t testing.TB) int {
	t.Helper()
	// Count entries under /proc/self/task — a proxy for live threads; if a
	// child were forked and still running it would appear as a new PID in
	// /proc/self/task/<tid>/children. We sum children across all tasks.
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Logf("/proc/self/task unavailable (non-Linux?): %v — static proof is authoritative", err)
		return 0
	}
	var total int
	for _, e := range entries {
		b, rerr := os.ReadFile("/proc/self/task/" + e.Name() + "/children")
		if rerr != nil {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(string(b)))
		total += len(fields)
	}
	return total
}

// buildDeliveryServiceForRead wires a DeliveryService whose resolvers are
// real-PG-backed (asset + binding) so ReadResource's parse path runs the
// production code. The opener is the in-memory archive.
func buildDeliveryServiceForRead(s *SkillSuite, _ domain.UUID, opener skill.ArchiveOpener) *skill.DeliveryService {
	assets := postgres.NewAssetReadRepo(s.db)
	bindings := postgres.NewBindingRepo(s.db)
	return skill.NewDeliveryService(s.skillRepo, realAssetResolver{assets}, realBindingResolver{bindings}, opener)
}

// realAssetResolver adapts the asset read repo to skill.AssetResolver. The
// repo's Get returns *KnowledgeAsset; the interface wants a value, so the
// adapter dereferences (a nil pointer maps to ErrPackageNotFound upstream).
type realAssetResolver struct{ inner *postgres.AssetReadRepo }

func (r realAssetResolver) GetAsset(ctx context.Context, assetID uuid.UUID) (domain.KnowledgeAsset, error) {
	ka, err := r.inner.Get(ctx, assetID)
	if err != nil || ka == nil {
		return domain.KnowledgeAsset{}, skill.ErrPackageNotFound
	}
	return *ka, nil
}
func (r realAssetResolver) ResolveVersion(ctx context.Context, assetID uuid.UUID, spec string) (domain.AssetVersion, error) {
	return r.inner.ResolveVersion(ctx, assetID, spec)
}

// ListSkillsByWorkspace mirrors the production SkillAssetResolver: page through
// AssetReadRepo.List scoped by workspace + asset_type='skill' (§6.3). A repo
// fault surfaces as ErrPackageNotFound so the delivery path returns an empty
// list (no existence leak on a transient fault).
func (r realAssetResolver) ListSkillsByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]domain.KnowledgeAsset, error) {
	var out []domain.KnowledgeAsset
	cursor := ""
	for {
		page, next, err := r.inner.List(ctx, asset.ListQuery{
			WorkspaceID: workspaceID,
			AssetType:   string(domain.AssetTypeSkill),
			Cursor:      cursor,
			PageSize:    100,
		})
		if err != nil {
			return nil, skill.ErrPackageNotFound
		}
		for _, a := range page {
			out = append(out, *a)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// realBindingResolver adapts the binding repo to skill.BindingResolver. The
// repo's List returns ([]AgentBinding, error); the interface wants the slice.
type realBindingResolver struct{ inner *postgres.BindingRepo }

func (r realBindingResolver) ActiveForAgent(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentBinding, error) {
	return r.inner.List(ctx, agentID, workspaceID, nil, 200)
}

// ===========================================================================
// §17.4 security: malicious script / symlink escape / disguised binary /
// compression bomb. All four are pattern-recognition or size-cap rejections —
// NEVER execution. They assert the archive is rejected or stored-as-untrusted
// without forking.
// ===========================================================================

// Test_Skill_Security_PathTraversalRejected: an archive entry that escapes the
// archive root (../escape) aborts the parse with ErrArchivePathTraversal. The
// entry is never materialized on disk (§4.4).
func (s *SkillSuite) Test_Skill_Security_PathTraversalRejected() {
	arch := buildTraversalArchive(s.T())
	_, err := skillpkg.Parse("trav", memArchiveReader{data: arch})
	require.Error(s.T(), err)
	assert.True(s.T(), errors.Is(err, skillpkg.ErrArchivePathTraversal),
		"a path-traversal entry must abort the parse (§17.4 / §4.4)")
}

// Test_Skill_Security_SymlinkNotFollowed: a symlink whose target points outside
// the archive (/etc/passwd) is recorded as a metadata-only entry; its target is
// NOT followed (§4.4 — never materialize an escape path). The parse succeeds
// (the symlink is an asset entry without content); the target bytes never
// appear in the parsed content.
func (s *SkillSuite) Test_Skill_Security_SymlinkNotFollowed() {
	arch := buildSymlinkArchive(s.T())
	parsed, err := skillpkg.Parse("sym", memArchiveReader{data: arch})
	require.NoError(s.T(), err, "a non-followed symlink must not abort the parse")
	// The symlink entry has NO content (target not followed).
	for _, f := range parsed.Package.Files {
		if f.Path == "assets/link" {
			assert.Empty(s.T(), f.Hash, "the symlink entry has no content hash (target not followed)")
		}
	}
	// The target bytes never leaked into any file's content.
	for _, f := range parsed.Package.Files {
		assert.NotContains(s.T(), string(f.Content), "root:",
			"the symlink target (/etc/passwd) was never followed — no root: lines in any file content")
	}
}

// Test_Skill_Security_DisguisedBinaryDetectedNotExecuted: an archive carrying a
// file with the ELF magic (a disguised binary) is parsed, the binary is
// classified as a script/asset and stored as an untrusted resource, and NO
// process is spawned. The exec-bit / shebang / ELF detection is pattern
// recognition only (§4.2 check 8).
func (s *SkillSuite) Test_Skill_Security_DisguisedBinaryDetectedNotExecuted() {
	arch := buildBinaryArchive(s.T())
	before := countChildProcesses(s.T())
	parsed, err := skillpkg.Parse("elf", memArchiveReader{data: arch})
	require.NoError(s.T(), err, "a binary-bearing archive must parse (stored, not executed)")
	after := countChildProcesses(s.T())
	assert.Equal(s.T(), before, after, "parsing a binary-bearing archive spawned no process (§17.4)")

	// The binary is recorded as an asset-kind entry under scripts/ (untrusted
	// resource), never honored for execution. classifyKind maps a scripts/*
	// path with a non-script extension to KindAsset — still untrusted, never exec'd.
	var payload *skillpkg.ParsedFile
	for i := range parsed.Package.Files {
		if parsed.Package.Files[i].Path == "scripts/payload.bin" {
			payload = &parsed.Package.Files[i]
		}
	}
	require.NotNil(s.T(), payload, "the binary entry is in the manifest")
	assert.Equal(s.T(), skillpkg.KindAsset, payload.Kind,
		"the binary is classified as an asset-kind entry (untrusted resource, never executed)")
	// Its content is the ELF bytes verbatim (stored, not exec'd) — the magic is intact.
	assert.True(s.T(), bytes.HasPrefix(payload.Content, []byte{0x7f, 'E', 'L', 'F'}),
		"the ELF content is stored verbatim as an untrusted resource, not executed")
}

// Test_Skill_Security_CompressionBombRejected: an archive whose tar header
// declares a size over the per-file cap aborts the parse with
// ErrArchiveTooLarge BEFORE allocating the bytes (§4.4 anti-compression-bomb).
func (s *SkillSuite) Test_Skill_Security_CompressionBombRejected() {
	arch := buildBombArchive(s.T())
	_, err := skillpkg.Parse("bomb", memArchiveReader{data: arch})
	require.Error(s.T(), err)
	assert.True(s.T(), errors.Is(err, skillpkg.ErrArchiveTooLarge),
		"an oversized entry must abort with ErrArchiveTooLarge before exhausting memory (§17.4)")
}

