package skill

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/lynn901/mora/internal/domain"
)

// --- fakes ---

// memRepo is an in-memory Repository so the service can be exercised without
// a DB (same precedent as binding's fakes). It stores packages by asset_version_id.
type memRepo struct {
	mu       sync.Mutex
	packages map[uuid.UUID]domain.SkillPackage
	saveErr  error
}

func newMemRepo() *memRepo {
	return &memRepo{packages: map[uuid.UUID]domain.SkillPackage{}}
}

func (r *memRepo) Get(_ context.Context, id uuid.UUID) (domain.SkillPackage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.packages[id]
	if !ok {
		return domain.SkillPackage{}, errors.New("not found")
	}
	return p, nil
}

func (r *memRepo) Save(_ context.Context, pkg domain.SkillPackage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return r.saveErr
	}
	r.packages[pkg.AssetVersionID] = pkg
	return nil
}

func (r *memRepo) UpdateValidationReport(_ context.Context, id uuid.UUID, status domain.SkillValidationStatus, report domain.ValidationReport, scanner string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.packages[id]
	if !ok {
		return errors.New("not found")
	}
	p.ValidationStatus = status
	p.ValidationReport = report
	p.ScannerVersion = scanner
	r.packages[id] = p
	return nil
}

// memOpener is an ArchiveOpener over an in-memory byte slice. It returns the
// archive bytes AS STORED — Mora never synthesizes an executable bit here.
type memOpener struct{ data []byte }

func (m memOpener) OpenArchive(_ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.data)), nil
}

// --- tests ---

// TestImport_Export_Roundtrip is the DoD §9 gate at the service level: a
// standard sample package import→export with content_hash consistent, file
// list consistent, unknown frontmatter fields preserved.
func TestImport_Export_Roundtrip(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(repo)

	arch := buildSampleArchive(t)
	assetVerID := uuid.New()

	res, err := svc.Import(context.Background(), AuthContext{IsAdmin: true},
		ImportOptions{AssetVersionID: assetVerID, StorageKey: "skill/echo.tar.gz"}, memOpener{arch})
	require.NoError(t, err)
	assert.Equal(t, domain.SkillValidationPassed, res.Package.ValidationStatus,
		"clean agentskills package → passed (saveable, NOT executable)")
	assert.Equal(t, domain.DeliveryLossless, res.Package.CompatibilityReport.Delivery)

	// Unknown legal field preserved verbatim (§2.3).
	require.Contains(t, res.Package.OriginalFrontmatter, "custom_runtime_config")

	// Export → content_hash must equal import content_hash (§9 gate).
	out, err := svc.Export(context.Background(), AuthContext{IsAdmin: true}, assetVerID, memOpener{arch})
	require.NoError(t, err)
	assert.Equal(t, res.ContentHash, out.ContentHash, "export content_hash == import content_hash (§9 往返门禁)")
	assert.NotEmpty(t, out.Archive)
}

// TestImport_OpaqueProfile — an upload with no declared format and no
// frontmatter format field is opaque → validation_status=opaque.
func TestImport_OpaqueProfile(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(repo)

	// SKILL.md with no format field → opaque.
	arch := buildOpaqueArchive(t)
	assetVerID := uuid.New()
	res, err := svc.Import(context.Background(), AuthContext{IsAdmin: true},
		ImportOptions{AssetVersionID: assetVerID, StorageKey: "skill/opaque.tar.gz"}, memOpener{arch})
	require.NoError(t, err)
	assert.Equal(t, domain.SkillValidationOpaque, res.Package.ValidationStatus,
		"opaque profile → validation_status=opaque")
	assert.Equal(t, domain.DeliveryIncompatible, res.Package.CompatibilityReport.Delivery)
	assert.Equal(t, domain.SkillFormatOpaque, res.Package.FormatID)
}

// TestImport_BlockFinding_Failed — a package with a structural defect (no
// SKILL.md) surfaces as ErrInvalidPackage and never persists.
func TestImport_BlockFinding_Failed(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(repo)
	arch := buildNoSkillMDArchive(t)
	_, err := svc.Import(context.Background(), AuthContext{IsAdmin: true},
		ImportOptions{AssetVersionID: uuid.New(), StorageKey: "skill/bad.tar.gz"}, memOpener{arch})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPackage))
	assert.Len(t, repo.packages, 0, "invalid package not persisted")
}

// TestImport_CompressionBomb — an oversized entry aborts as ErrArchiveTooLarge.
func TestImport_CompressionBomb(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(repo)
	arch := buildBombArchive(t)
	_, err := svc.Import(context.Background(), AuthContext{IsAdmin: true},
		ImportOptions{AssetVersionID: uuid.New(), StorageKey: "skill/bomb.tar.gz"}, memOpener{arch})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrArchiveTooLarge))
}

// TestRevalidate — re-running validation on a stored package updates the
// report + status without re-importing.
func TestRevalidate(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(repo)
	arch := buildSampleArchive(t)
	assetVerID := uuid.New()
	_, err := svc.Import(context.Background(), AuthContext{IsAdmin: true},
		ImportOptions{AssetVersionID: assetVerID, StorageKey: "skill/echo.tar.gz"}, memOpener{arch})
	require.NoError(t, err)

	pkg, err := svc.Revalidate(context.Background(), AuthContext{IsAdmin: true}, assetVerID, memOpener{arch})
	require.NoError(t, err)
	assert.Equal(t, domain.SkillValidationPassed, pkg.ValidationStatus)
	assert.Equal(t, ScannerVersion, pkg.ScannerVersion)
}

// TestExport_Mismatch — if the stored manifest's content_hash does not match
// the re-derived one, export returns ErrRoundtripMismatch.
func TestExport_Mismatch(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(repo)
	arch := buildSampleArchive(t)
	assetVerID := uuid.New()
	_, err := svc.Import(context.Background(), AuthContext{IsAdmin: true},
		ImportOptions{AssetVersionID: assetVerID, StorageKey: "skill/echo.tar.gz"}, memOpener{arch})
	require.NoError(t, err)
	// Tamper the stored manifest content_hash (the roundtrip anchor the
	// export path recomputes against). The top-level ContentHash is a copy;
	// the export gate recomputes from file content and compares to the
	// manifest's ContentHash, so tampering the manifest anchor is what
	// forces ErrRoundtripMismatch.
	repo.mu.Lock()
	p := repo.packages[assetVerID]
	p.Manifest.ContentHash = "tampered"
	p.ContentHash = "tampered"
	repo.packages[assetVerID] = p
	repo.mu.Unlock()
	_, err = svc.Export(context.Background(), AuthContext{IsAdmin: true}, assetVerID, memOpener{arch})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRoundtripMismatch))
}

// TestExport_NotFound — exporting a non-existent package → ErrPackageNotFound.
func TestExport_NotFound(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(repo)
	_, err := svc.Export(context.Background(), AuthContext{IsAdmin: true}, uuid.New(), memOpener{buildSampleArchive(t)})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPackageNotFound))
}
