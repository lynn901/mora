package skill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/skill/compatibility"
	"github.com/lynn901/mora/internal/module/skill/package"
	"github.com/lynn901/mora/internal/module/skill/validate"
)

// Service is the Skill package governance application service (design-docs/19
// §2/§3.1/§4 — Phase 5-2, YS-161). It composes the three sub-domains into the
// import → validate → persist + export pipeline and owns the storage port.
//
// The service stays pgx-free: archive bytes come from the ArchiveOpener seam
// (MinIO-backed, wired by sub-task D), persistence goes through the
// Repository port (pgx impl in infra/postgres, sub-task D). The sub-domains
// (package/compatibility/validate) are pure and in-process — they never
// execute a script (§4.4 script-execution count = 0 across all four paths).
//
// RBAC: import/validate/export are management operations gated on the
// `assign` action on the workspace (§6.1). The engine is nil in tests only;
// production wiring MUST chain WithAuthz. (Wiring lands with the REST handler
// in sub-task D; the service exposes the authz seam now so D is mechanical.)
type Service struct {
	repo Repository
	rbac rbacEngine // nil = no resource-level authz (dev/test only)
	now  func() time.Time
}

// rbacEngine is the minimal authz seam the service needs (the full
// rbac.Engine satisfies it via the SkillAuthz adapter wired in sub-task D).
// Kept as an interface so the service does not import platform/rbac directly
// here — production wires the real engine via WithAuthz in sub-task D,
// mirroring module/binding's WithAuthz.
type rbacEngine interface {
	// CheckAssign asserts the caller may `assign` on the workspace that owns
	// assetVersionID. The adapter resolves asset_version_id → workspace_id
	// (mirrors the memory-unit ACL gate: resolve asset id before Check, YS-146).
	// A denial returns ErrPackageNotFound (no existence leak, same convention
	// as the binding service). IsAdmin / nil-engine short-circuits to allow.
	CheckAssign(ctx context.Context, assetVersionID uuid.UUID, subject domain.SubjectType, principalID uuid.UUID, groupIDs []uuid.UUID, isAdmin bool) error
}

// AuthContext carries the caller identity for RBAC (mirrors
// binding.AuthContext).
type AuthContext struct {
	SubjectType domain.SubjectType
	PrincipalID uuid.UUID
	GroupIDs    []uuid.UUID
	IsAdmin     bool
}

// NewService wires the skill governance service. rbac is nil by design;
// production wiring MUST chain WithAuthz.
func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: time.Now().UTC}
}

// WithAuthz injects the RBAC engine (mirrors binding.Service.WithAuthz).
// Sub-task D wires the real rbac.Engine here; until then the service runs
// unauth (tests only).
func (s *Service) WithAuthz(engine rbacEngine) *Service {
	s.rbac = engine
	return s
}

// openerAdapter adapts the service's ArchiveOpener seam to the package
// sub-domain's ArchiveReader interface. They have the same shape; the
// adapter exists so the two packages do not import each other (the three
// sub-domains + the service are independent compilation units).
type openerAdapter struct{ opener ArchiveOpener }

func (a openerAdapter) Open(storageKey string) (io.ReadCloser, error) {
	return a.opener.OpenArchive(storageKey)
}

// Import runs the full import pipeline (§2 / §4): parse → classify →
// validate → persist. It is the single composition of the three sub-domains.
//
//  1. package.Parse streams the archive (size-capped, in-memory, no exec bit
//     on disk), builds the manifest + content_hash, preserves frontmatter.
//  2. compatibility.Determine classifies the profile + builds the
//     compatibility_report.
//  3. validate.Run runs the 8 static checks + rolls up validation_status.
//  4. Repository.Save persists the skill_packages row in one write.
//
// Authorization: the caller must hold `assign` on the workspace that owns the
// asset version (§6.1 管理型). A denial returns ErrPackageNotFound (no leak).
func (s *Service) Import(ctx context.Context, auth AuthContext, opts ImportOptions, opener ArchiveOpener) (ImportResult, error) {
	if err := s.requireAuthorized(ctx, auth, opts.AssetVersionID); err != nil {
		return ImportResult{}, err
	}
	if opts.StorageKey == "" || opener == nil {
		return ImportResult{}, ErrInvalidPackage
	}

	// 1. Parse.
	parsed, err := skillpkg.Parse(opts.StorageKey, openerAdapter{opener})
	if err != nil {
		return ImportResult{}, mapParseErr(err)
	}

	// Resolve the format_id: caller override > frontmatter-declared > opaque.
	formatID := domain.SkillFormatID(opts.DeclaredFormatID)
	if formatID == "" {
		formatID = domain.SkillFormatID(parsed.Package.DeclaredFormatID)
	}
	if formatID == "" {
		formatID = domain.SkillFormatOpaque
	}
	schemaVer := parsed.Package.DeclaredSchemaVer
	if schemaVer == "" {
		schemaVer = compatibility.SpecBaseline
	}

	// 2. Classify + compatibility report.
	profile := compatibility.Classify(string(formatID))
	compReport := compatibility.Determine(compatibility.ReportInput{
		Profile:          profile,
		FormatID:         string(formatID),
		Frontmatter:      parsed.Package.OriginalFrontmatter,
		DeclaredRuntime:  declaredRuntime(parsed.Package.OriginalFrontmatter),
	})

	// 3. Validate (with a hash provider backed by the parsed content).
	hp := parsedHashProvider{files: parsed.Package.Files}
	valReport, status := validate.Run(validate.Input{
		Manifest:            parsed.Manifest,
		FrontmatterParseErr: parsed.Package.FrontmatterParseErr,
		Profile:             profile,
		ContentProvider:    hp,
	})

	// An opaque profile forces status=opaque even if structure passed.
	now := s.now()
	pkg := domain.SkillPackage{
		AssetVersionID:       opts.AssetVersionID,
		StorageKey:           opts.StorageKey,
		FormatID:             formatID,
		SchemaVersion:        schemaVer,
		Manifest:             parsed.Manifest,
		OriginalFrontmatter: parsed.Package.OriginalFrontmatter,
		ContentHash:          parsed.ContentHash,
		ValidationStatus:     status,
		ValidationReport:    valReport,
		CompatibilityReport: compReport,
		ScannerVersion:       ScannerVersion,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	// 4. Persist.
	if err := s.repo.Save(ctx, pkg); err != nil {
		return ImportResult{}, fmt.Errorf("skill: persist package: %w", err)
	}
	return ImportResult{Package: pkg, ContentHash: parsed.ContentHash}, nil
}

// Revalidate re-runs the static validator on a stored package (the :validate
// route). It re-reads the archive to recompute hashes (§4.2 check 5). A nil
// opener skips hash recomputation (info finding) — used when the archive is
// not available.
func (s *Service) Revalidate(ctx context.Context, auth AuthContext, assetVersionID uuid.UUID, opener ArchiveOpener) (domain.SkillPackage, error) {
	if err := s.requireAuthorized(ctx, auth, assetVersionID); err != nil {
		return domain.SkillPackage{}, err
	}
	pkg, err := s.repo.Get(ctx, assetVersionID)
	if err != nil {
		return domain.SkillPackage{}, ErrPackageNotFound
	}

	var hp validate.HashProvider
	if opener != nil {
		// Re-parse to get fresh content for hash recomputation. This reuses
		// the import parse path — script-execution count stays 0 (§4.4).
		parsed, perr := skillpkg.Parse(pkg.StorageKey, openerAdapter{opener})
		if perr == nil {
			hp = parsedHashProvider{files: parsed.Package.Files}
		}
		// On parse error, skip hash recompute (info finding) rather than fail
		// the whole revalidation — the original package is still saveable.
	}
	profile := compatibility.Classify(string(pkg.FormatID))
	valReport, status := validate.Run(validate.Input{
		Manifest:          pkg.Manifest,
		Profile:          profile,
		ContentProvider:  hp,
	})

	if err := s.repo.UpdateValidationReport(ctx, assetVersionID, status, valReport, ScannerVersion); err != nil {
		return domain.SkillPackage{}, fmt.Errorf("skill: update validation report: %w", err)
	}
	pkg.ValidationStatus = status
	pkg.ValidationReport = valReport
	pkg.ScannerVersion = ScannerVersion
	return pkg, nil
}

// GetPackage returns the stored skill_packages row for an asset version (the
// §6.1 GET version route). It is a READ path: it does NOT re-run validation
// (use Revalidate for that). Authorization: the caller must hold `assign` on
// the workspace that owns the asset version (§6.1 management visibility — the
// skill_packages view is management, not the default Agent tool set). A denial
// returns ErrPackageNotFound (no existence leak).
func (s *Service) GetPackage(ctx context.Context, auth AuthContext, assetVersionID uuid.UUID) (domain.SkillPackage, error) {
	if err := s.requireAuthorized(ctx, auth, assetVersionID); err != nil {
		return domain.SkillPackage{}, err
	}
	pkg, err := s.repo.Get(ctx, assetVersionID)
	if err != nil {
		return domain.SkillPackage{}, ErrPackageNotFound
	}
	return pkg, nil
}

// Export losslessly re-derives the archive from the stored manifest and
// asserts the content_hash equals the import content_hash (§9 往返门禁).
// The opener yields the file content for each manifest entry (the MinIO-
// backed original reader, wired by sub-task D).
func (s *Service) Export(ctx context.Context, auth AuthContext, assetVersionID uuid.UUID, opener ArchiveOpener) (ExportOutput, error) {
	if err := s.requireAuthorized(ctx, auth, assetVersionID); err != nil {
		return ExportOutput{}, err
	}
	pkg, err := s.repo.Get(ctx, assetVersionID)
	if err != nil {
		return ExportOutput{}, ErrPackageNotFound
	}
	if opener == nil {
		return ExportOutput{}, ErrInvalidPackage
	}

	// Build a content provider that pulls each file's bytes from the archive
	// via Parse (re-using the import parse path — exec count stays 0).
	cp, err := archiveContentProvider(pkg.StorageKey, opener)
	if err != nil {
		return ExportOutput{}, fmt.Errorf("skill: export open archive: %w", err)
	}
	res, err := skillpkg.ExportFromManifest(pkg.Manifest, cp)
	if err != nil {
		if errors.Is(err, skillpkg.ErrExportMismatch) {
			return ExportOutput{}, ErrRoundtripMismatch
		}
		return ExportOutput{}, err
	}
	return ExportOutput{Archive: res.Archive, ContentHash: res.ContentHash}, nil
}

// archiveContentProvider parses the archive once and yields file content by
// path, so ExportFromManifest can re-package without re-exec'ing anything.
type archiveContent struct {
	files map[string][]byte
}

func (a archiveContent) Content(p string) ([]byte, error) {
	b, ok := a.files[p]
	if !ok {
		return nil, fmt.Errorf("file not in archive: %s", p)
	}
	return b, nil
}

func archiveContentProvider(storageKey string, opener ArchiveOpener) (skillpkg.ContentProvider, error) {
	parsed, err := skillpkg.Parse(storageKey, openerAdapter{opener})
	if err != nil {
		return nil, err
	}
	m := make(map[string][]byte, len(parsed.Package.Files))
	for _, f := range parsed.Package.Files {
		if f.Hash != "" { // skip symlink entries (no content)
			m[f.Path] = f.Content
		}
	}
	return archiveContent{files: m}, nil
}

// parsedHashProvider adapts the parsed file list to validate.HashProvider
// so the validator can recompute hashes without re-reading the archive.
type parsedHashProvider struct{ files []skillpkg.ParsedFile }

func (p parsedHashProvider) Content(path string) ([]byte, error) {
	for _, f := range p.files {
		if f.Path == path {
			return f.Content, nil
		}
	}
	return nil, fmt.Errorf("file not in archive: %s", path)
}

// mapParseErr translates package.Parse errors into service sentinel errors.
func mapParseErr(err error) error {
	switch {
	case errors.Is(err, skillpkg.ErrArchiveTooLarge),
		errors.Is(err, skillpkg.ErrTooManyEntries):
		return fmt.Errorf("%w: %v", ErrArchiveTooLarge, err)
	case errors.Is(err, skillpkg.ErrMissingSkillMD),
		errors.Is(err, skillpkg.ErrNotTarGz),
		errors.Is(err, skillpkg.ErrArchivePathTraversal),
		errors.Is(err, skillpkg.ErrDuplicateEntry):
		return fmt.Errorf("%w: %v", ErrInvalidPackage, err)
	default:
		return fmt.Errorf("skill: parse: %w", err)
	}
}

// declaredRuntime extracts a declared runtime from the frontmatter (if any),
// for the compatibility report's runtime_needs list. The spec allows a
// `runtime` field; hermes packages declare one explicitly.
func declaredRuntime(fm map[string]any) string {
	if fm == nil {
		return ""
	}
	if r, ok := fm["runtime"].(string); ok {
		return r
	}
	return ""
}

// requireAuthorized gates management operations on the `assign` action on the
// workspace that owns the asset version. A nil engine (tests) allows. The
// workspace id is resolved by the RBAC layer from the asset version; the
// adapter (sub-task D) resolves asset_version_id → workspace_id, then runs
// rbac.Engine.Check (mirrors the memory-unit ACL gate pattern: resolve asset
// id before Check, YS-146).
func (s *Service) requireAuthorized(ctx context.Context, auth AuthContext, assetVersionID uuid.UUID) error {
	if s.rbac == nil || auth.IsAdmin {
		return nil
	}
	return s.rbac.CheckAssign(ctx, assetVersionID, auth.SubjectType, auth.PrincipalID, auth.GroupIDs, auth.IsAdmin)
}
