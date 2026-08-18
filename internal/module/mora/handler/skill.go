package handler

// skill.go is the HTTP adapter for the Skill package governance REST control
// plane (design-docs/19 §6.1 — Phase 5-3, YS-163). It mirrors source.go /
// asset.go: the handler only binds HTTP ↔ service inputs and maps service
// errors to the §11.4 envelope. Business logic + RBAC stay in the skill
// service (internal/module/skill); the pgx repository + the MinIO archive
// opener live in internal/infra/postgres.
//
// Existence never leaks (§8.2 / §1.2): every read path maps
// skill.ErrPackageNotFound to the same 404 + 40400 the response package emits
// for a permission denial, so a caller cannot tell a missing skill package
// from a not-allowed one. Management operations (import / validate / export)
// are gated on the `assign` action on the workspace (§6.1 管理型) and do NOT
// enter the default Agent MCP tool set (§6.3).

import (
	"io"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
	skill "github.com/lynn901/mora/internal/module/skill"
)

// SkillHandler exposes the Skill package governance REST endpoints (§6.1).
type SkillHandler struct {
	svc       *skill.Service
	registrar skill.SkillAssetRegistrar
	opener    skill.ArchiveOpener
}

// NewSkillHandler wires the Skill governance handler. registrar creates the
// asset+version rows on import; opener streams the immutable archive original
// (MinIO-backed) for parse/validate/export. Both are infra-owned seams the
// service stays free of.
func NewSkillHandler(svc *skill.Service, registrar skill.SkillAssetRegistrar, opener skill.ArchiveOpener) *SkillHandler {
	return &SkillHandler{svc: svc, registrar: registrar, opener: opener}
}

// --- POST /workspaces/:workspace_id/knowledge/assets (asset_type=skill 导入) ---

// importSkillReq carries the form-field overrides for a skill import. The
// SKILL.md frontmatter is the authoritative source for name/description/
// version; these fields override the frontmatter when the caller sets them
// (e.g. a package whose frontmatter is missing a name). The archive itself is
// a multipart file, not a JSON field.
type importSkillReq struct {
	Name        string `form:"name"`
	Description string `form:"description"`
	Version     string `form:"version"`
}

func (h *SkillHandler) Import(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	// asset_type=skill is the only type this handler imports; the route is
	// shared with future asset-type imports but the skill handler answers the
	// ?asset_type=skill (or multipart skill upload) form only.
	file, err := c.FormFile("archive")
	if err != nil {
		response.Fail(c, badRequestErr("missing archive file"))
		return
	}
	// Size cap: the parse path enforces MaxPerFileSize; gate at the HTTP layer
	// too so a huge upload fails fast rather than buffering into memory.
	const maxArchiveBytes = 64 << 20 // 64 MiB — matches the skill package size cap (§4.4)
	if file.Size > maxArchiveBytes {
		response.Fail(c, badRequestErr("archive exceeds 64MiB size cap"))
		return
	}
	src, err := file.Open()
	if err != nil {
		response.Fail(c, badRequestErr("cannot open archive"))
		return
	}
	defer src.Close()
	archive, err := io.ReadAll(src)
	if err != nil {
		response.Fail(c, badRequestErr("cannot read archive"))
		return
	}
	var req importSkillReq
	if err := c.ShouldBind(&req); err != nil {
		response.Fail(c, badRequestErr("invalid form fields"))
		return
	}
	auth := MustAuth(c)

	// 1. Register the asset + version rows + store the archive in MinIO.
	//    CreatedBy attributes the import to the acting principal (a user JWT
	//    caller → user; a delegated agent → agent).
	reg, err := h.registrar.RegisterSkillAsset(c.Request.Context(), skill.RegisterSkillInput{
		WorkspaceID: wsID,
		Name:        req.Name,
		Description: req.Description,
		Version:     req.Version,
		Archive:     archive,
		CreatedBy:   domain.EventActor{Type: principalSubjectType(auth), ID: principalID(auth)},
	})
	if err != nil {
		response.Fail(c, mapSkillErr(err))
		return
	}

	// 2. Parse + classify + validate + persist the skill_packages row.
	//    ImportOptions.DeclaredFormatID is empty → the service infers the
	//    profile from the SKILL.md frontmatter (§2.2), else opaque.
	res, err := h.svc.Import(c.Request.Context(), skillAuth(auth), skill.ImportOptions{
		AssetVersionID: reg.AssetVersionID,
		StorageKey:     reg.StorageKey,
	}, h.opener)
	if err != nil {
		response.Fail(c, mapSkillErr(err))
		return
	}
	// Mirror GetVersion's response shape: a flat set of governance fields PLUS
	// the full skill_packages object under the "skill_packages" key, so the
	// Import and GetVersion envelopes are consistent (YS-163 DEFECT-3: Import
	// previously returned only flat fields, diverging from GetVersion).
	pkg := res.Package
	response.Created(c, gin.H{
		"asset_id":          reg.AssetID,
		"asset_version_id":  pkg.AssetVersionID,
		"storage_key":       pkg.StorageKey,
		"content_hash":      res.ContentHash,
		"format_id":         string(pkg.FormatID),
		"validation_status": string(pkg.ValidationStatus),
		"validation_report": pkg.ValidationReport,
		"compatibility_report": pkg.CompatibilityReport,
		"skill_packages":    pkg, // the full skill_packages清单 (§6.1, matches GetVersion)
	})
}

// --- GET /knowledge/assets/:id/versions/:vid (含 skill_packages 清单) ---

func (h *SkillHandler) GetVersion(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	vid, err := uuid.Parse(c.Param("vid"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid vid"))
		return
	}
	// The skill package mounts 1:1 on the asset version; the version id is the
	// skill_packages PK. This is a READ path (no re-validation): it returns the
	// stored skill_packages row. The caller's asset-level read permission is
	// enforced by the asset handler's Get; the skill service gates this on the
	// `assign` action on the workspace (management visibility — §6.1 marks the
	// skill_packages view as management, not the default Agent tool set).
	_ = id
	got, err := h.svc.GetPackage(c.Request.Context(), skillAuth(MustAuth(c)), vid)
	if err != nil {
		response.Fail(c, mapSkillErr(err))
		return
	}
	response.OK(c, gin.H{
		"asset_version_id":     got.AssetVersionID,
		"format_id":            string(got.FormatID),
		"schema_version":       got.SchemaVersion,
		"manifest":             got.Manifest,
		"original_frontmatter": got.OriginalFrontmatter,
		"content_hash":         got.ContentHash,
		"signature":            got.Signature,
		"provenance_ref":       got.ProvenanceRef,
		"validation_status":    string(got.ValidationStatus),
		"validation_report":    got.ValidationReport,
		"compatibility_report": got.CompatibilityReport,
		"scanner_version":      got.ScannerVersion,
		"created_at":           got.CreatedAt,
		"updated_at":           got.UpdatedAt,
		"skill_packages":       got, // the full skill_packages清单 (§6.1)
	})
}

// --- POST /knowledge/assets/:id/versions/:vid/validate (触发/重跑静态校验) ---

func (h *SkillHandler) Validate(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	vid, err := uuid.Parse(c.Param("vid"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid vid"))
		return
	}
	_ = id
	pkg, err := h.svc.Revalidate(c.Request.Context(), skillAuth(MustAuth(c)), vid, h.opener)
	if err != nil {
		response.Fail(c, mapSkillErr(err))
		return
	}
	response.OK(c, gin.H{
		"asset_version_id":  pkg.AssetVersionID,
		"validation_status": string(pkg.ValidationStatus),
		"validation_report": pkg.ValidationReport,
		"scanner_version":    pkg.ScannerVersion,
	})
}

// --- GET /knowledge/assets/:id/versions/:vid/export (无损导出，往返 hash 校验) ---

func (h *SkillHandler) Export(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	vid, err := uuid.Parse(c.Param("vid"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid vid"))
		return
	}
	_ = id
	out, err := h.svc.Export(c.Request.Context(), skillAuth(MustAuth(c)), vid, h.opener)
	if err != nil {
		response.Fail(c, mapSkillErr(err))
		return
	}
	// §9 roundtrip gate: the exported content_hash MUST equal the import
	// content_hash. The service already asserted this (ErrRoundtripMismatch on
	// mismatch); echo both in the response so the caller can verify.
	c.Header("X-Content-Hash", out.ContentHash)
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", `attachment; filename="skill-export.tar.gz"`)
	c.Header("Content-Length", strconv.Itoa(len(out.Archive)))
	c.Data(200, "application/gzip", out.Archive)
}

// --- helpers ---

// mapSkillErr maps a skill-service error to the §11.4 envelope. The key
// invariant (§8.2): a missing/unreadable skill package returns NotFound
// (404 + 40400) — the SAME shape a permission denial takes — so existence
// never leaks. A structurally invalid import returns BadRequest (400). A
// roundtrip hash mismatch returns Conflict (409) — the stored package is
// inconsistent with its archive (a delivery-blocking integrity failure).
func mapSkillErr(err error) error {
	switch {
	case pkgerr.Is(err, skill.ErrPackageNotFound):
		return pkgerr.NotFound("not found")
	case pkgerr.Is(err, skill.ErrInvalidPackage),
		pkgerr.Is(err, skill.ErrArchiveTooLarge):
		return pkgerr.BadRequest("invalid package")
	case pkgerr.Is(err, skill.ErrRoundtripMismatch):
		return pkgerr.Conflict("roundtrip content_hash mismatch")
	}
	return err
}

// skillAuth maps the HTTP AuthState to the skill service's AuthContext (mirrors
// the source handler's srcAuth helper). The RBAC subject is the acting user
// (principalID); a service-account caller resolves to its service account id
// with no admin bypass. GroupIDs is plumbed so group-inherited grants apply.
func skillAuth(s AuthState) skill.AuthContext {
	return skill.AuthContext{
		SubjectType: principalSubjectType(s),
		PrincipalID: principalID(s),
		GroupIDs:    s.Groups,
		IsAdmin:     s.IsAdmin,
	}
}
