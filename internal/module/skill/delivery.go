package skill

// delivery.go is the §6.2 internal API (MCP → Mora) delivery path. It is the
// READ surface an Agent's MCP tool layer calls to obtain a Skill's SKILL.md
// header + resource manifest + compatibility_report, and to progressively read
// individual resource files — all trimmed by the agent's binding delivery_mode
// (tool / summary / inline).
//
// Layering: this service composes the skill Repository (the stored package),
// an AssetResolver (asset_id → type + workspace + version), a
// BindingResolver (agent → effective binding → delivery_mode), and the
// ArchiveOpener (progressive resource read). It stays pgx-free — the resolver
// seams are implemented in internal/infra/postgres and wired in main.go.
//
// Security invariants (§1.2 / §8.2):
//   - Existence never leaks: a caller whose agent has no allow binding for the
//     asset gets ErrPackageNotFound (→ 404, indistinguishable from a missing
//     skill). The internal API does NOT reveal that the skill exists at all.
//   - delivery_mode NARROWS the delivered surface: tool = full header +
//     manifest + compatibility (the agent sees the tool shape); summary =
//     header + capability_summary only (no raw resource list); inline = header
//     + the full manifest + per-file progressive reads. A deny binding yields
//     not-found regardless of mode.
//   - No script execution (§4.4): progressive reads stream archive bytes
//     through package.Parse; exec count stays 0. An exec bit detected in the
//     manifest is reported as metadata, NEVER honored.
//   - INTERNAL_SERVICE_TOKEN alone (no delegated agent context) cannot reach
//     this path: Delivery requires an AgentID (the binding resolution keys on
//     it). A service_account caller with no agent context → ErrPackageNotFound
//     (§11.2 — the token proves service identity only, never the end-principal).

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	skillpkg "github.com/lynn901/mora/internal/module/skill/package"
)

// AssetResolver resolves an asset id to its type + workspace + version, the
// shape the delivery path needs to (a) match asset_type-scope bindings and
// (b) resolve "latest" / a version number to a concrete asset_version_id. The
// asset read repo implements this; a missing asset returns a not-found error.
type AssetResolver interface {
	// GetAsset returns the knowledge asset (type + workspace_id) for assetID.
	GetAsset(ctx context.Context, assetID uuid.UUID) (domain.KnowledgeAsset, error)
	// GetVersion resolves a version spec for an asset to a concrete
	// asset_version_id + its build/governance status. versionSpec is either a
	// uuid (the version id directly) or "latest" / "" (the latest published
	// version — the §6.2 default for follow_published bindings). A missing
	// version returns a not-found error.
	ResolveVersion(ctx context.Context, assetID uuid.UUID, versionSpec string) (domain.AssetVersion, error)
	// ListSkillsByWorkspace returns the skill-typed knowledge assets in a
	// workspace (skill_list backing, §6.3). The repo MUST scope by workspace_id
	// + asset_type='skill' so a non-member never sees another workspace's
	// skills. An empty result is normal (no skills / no membership) — existence
	// does not leak because the agent-level binding gate then trims this to
	// only skills the agent is bound to (§8.2).
	ListSkillsByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]domain.KnowledgeAsset, error)
}

// BindingResolver resolves the effective binding governing one asset's
// delivery to one agent (§5.3). The binding module's Repository implements this
// via ActiveForAgent + the in-memory precedence the delivery service applies
// here; this seam keeps module/skill free of a direct binding.Repository
// import. A nil/missing allow binding returns ErrPackageNotFound (no leak).
//
// The resolver returns the raw candidate bindings; the delivery service picks
// the winner (§5.3 precedence: deny>allow, priority, scope-narrowness) so the
// policy lives in one testable place.
type BindingResolver interface {
	ActiveForAgent(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentBinding, error)
}

// DeliveryService is the §6.2 internal delivery surface. It composes the skill
// package repo, the asset resolver, the binding resolver, and the archive
// opener. It does NOT run authz itself — the caller (the internal API handler,
// wired under the same AuthMiddleware) already enforced that the bearer is a
// delegated agent context (AgentID + WorkspaceID). This service then enforces
// the AGENT-LEVEL gate: does this agent have an allow binding for this skill?
type DeliveryService struct {
	packages Repository
	assets   AssetResolver
	bindings BindingResolver
	opener   ArchiveOpener
}

// NewDeliveryService wires the delivery service. opener may be nil when only
// the manifest delivery (no progressive resource read) is needed; ReadResource
// fails closed with ErrInvalidPackage if opener is nil.
func NewDeliveryService(packages Repository, assets AssetResolver, bindings BindingResolver, opener ArchiveOpener) *DeliveryService {
	return &DeliveryService{packages: packages, assets: assets, bindings: bindings, opener: opener}
}

// DeliveryResult is the §6.2 GET /internal/v1/skills/{id}/versions/{version}
// response, trimmed by delivery_mode. The Header is always the SKILL.md
// frontmatter (name/description/version + preserved opaque fields). The
// Manifest is the full file inventory for tool/inline modes; summary mode gets
// the capability_summary projection only (no raw file list — the agent learns
// the skill's shape, not its bytes). CompatibilityReport is always present so
// the caller can surface runtime_needs / opaque_fields.
type DeliveryResult struct {
	AssetID            uuid.UUID                 `json:"asset_id"`
	AssetVersionID     uuid.UUID                 `json:"asset_version_id"`
	VersionNo          int64                     `json:"version_no"`
	DeliveryMode       domain.BindingDeliveryMode `json:"delivery_mode"`
	Header             map[string]any            `json:"header"`               // SKILL.md frontmatter
	Manifest           *domain.SkillManifest     `json:"manifest,omitempty"`   // nil in summary mode
	CapabilitySummary  map[string]any            `json:"capability_summary,omitempty"` // summary mode projection
	CompatibilityReport domain.CompatibilityReport `json:"compatibility_report"`
	ContentHash        string                    `json:"content_hash"`
}

// Deliver resolves the agent's effective binding for the skill, picks the
// version (binding pinned > caller versionSpec > latest published), and returns
// the delivery-mode-trimmed package surface. A caller whose agent has no allow
// binding gets ErrPackageNotFound (no existence leak — §8.2).
//
// agentID is the delegated context's AgentID (§11.2); a nil agentID (a
// service_account caller with no agent context) is refused — the internal token
// alone never authorizes skill delivery.
func (s *DeliveryService) Deliver(ctx context.Context, agentID, workspaceID uuid.UUID, assetID uuid.UUID, versionSpec string) (DeliveryResult, error) {
	if agentID == uuid.Nil {
		return DeliveryResult{}, ErrPackageNotFound
	}
	asset, err := s.assets.GetAsset(ctx, assetID)
	if err != nil {
		return DeliveryResult{}, ErrPackageNotFound
	}
	if asset.WorkspaceID != workspaceID {
		// Cross-workspace: the asset is not in the delegated workspace scope.
		// Surface not-found (no leak — the caller cannot tell a cross-workspace
		// skill from a missing one).
		return DeliveryResult{}, ErrPackageNotFound
	}

	// Resolve the effective binding (§5.3). A missing/deny → not-found.
	binding, err := s.resolveEffectiveBinding(ctx, agentID, workspaceID, assetID, asset.AssetType)
	if err != nil {
		return DeliveryResult{}, err
	}

	// Pick the version: pinned > caller spec > latest published.
	versionID := binding.PinnedVersionID
	if versionID == nil || *versionID == uuid.Nil {
		v, verr := s.assets.ResolveVersion(ctx, assetID, versionSpec)
		if verr != nil {
			return DeliveryResult{}, ErrPackageNotFound
		}
		// follow_published: a version not in published/ready state is not
		// deliverable (§5.1 — no silent fallback). The resolver returns only
		// published/ready for "latest"; an explicit version id is honored as-is
		// (the binding's pinnedVersionGate enforces usability at decision time).
		versionID = &v.ID
	}

	pkg, err := s.packages.Get(ctx, *versionID)
	if err != nil {
		return DeliveryResult{}, ErrPackageNotFound
	}

	// Trim by delivery_mode.
	res := DeliveryResult{
		AssetID:            assetID,
		AssetVersionID:     pkg.AssetVersionID,
		DeliveryMode:       binding.DeliveryMode,
		Header:             pkg.OriginalFrontmatter,
		CompatibilityReport: pkg.CompatibilityReport,
		ContentHash:        pkg.ContentHash,
	}
	switch binding.DeliveryMode {
	case domain.BindingDeliveryTool:
		// Full tool surface: header + manifest + compatibility. The agent sees
		// the complete file inventory (paths + hashes, no bytes — bytes come
		// via progressive ReadResource).
		m := pkg.Manifest
		res.Manifest = &m
	case domain.BindingDeliverySummary:
		// Summary only: header + capability_summary projection. No raw file
		// list — the agent learns the skill's declared shape, not its contents.
		res.CapabilitySummary = pkg.Manifest.CapabilitySummary
	case domain.BindingDeliveryInline:
		// Inline: header + manifest (the caller will progressively read
		// resources inline via ReadResource). Same surface as tool but the
		// delivery intent is "embed contents," recorded for the consumer.
		m := pkg.Manifest
		res.Manifest = &m
	default:
		// Unknown delivery_mode → fail closed (no surface). This is a data
		// invariant violation (the binding sink validates delivery_mode); a
		// defensive default-deny is safest.
		return DeliveryResult{}, ErrInvalidPackage
	}
	// The version_no is not on the skill package (it mounts on the version);
	// resolve it for the response envelope. A failure here is non-fatal —
	// the version_no is informational.
	if v, err := s.assets.ResolveVersion(ctx, assetID, pkg.AssetVersionID.String()); err == nil {
		res.VersionNo = v.VersionNo
	}
	return res, nil
}

// ReadResource progressively reads one resource file from the skill archive
// (§6.2 GET /internal/v1/skills/{id}/resources/{path}). It is gated by the same
// effective-binding resolution as Deliver: a caller with no allow binding, or
// a binding whose delivery_mode does not permit raw resource reads, gets
// ErrPackageNotFound (no leak). Per §6.2, inline mode permits progressive
// reads; tool mode permits reads of declared resources (the agent fetches
// bytes on demand); summary mode does NOT permit raw reads (the agent received
// only the summary). The path must match a manifest entry (no traversal).
func (s *DeliveryService) ReadResource(ctx context.Context, agentID, workspaceID uuid.UUID, assetID uuid.UUID, versionSpec, resourcePath string) (ResourceContent, error) {
	if agentID == uuid.Nil || s.opener == nil {
		return ResourceContent{}, ErrPackageNotFound
	}
	asset, err := s.assets.GetAsset(ctx, assetID)
	if err != nil {
		return ResourceContent{}, ErrPackageNotFound
	}
	if asset.WorkspaceID != workspaceID {
		return ResourceContent{}, ErrPackageNotFound
	}
	binding, err := s.resolveEffectiveBinding(ctx, agentID, workspaceID, assetID, asset.AssetType)
	if err != nil {
		return ResourceContent{}, err
	}
	// Summary mode does not permit raw resource reads — the agent received only
	// the capability summary, not the file inventory. Reading a resource would
	// leak the file's existence/contents beyond the granted surface.
	if binding.DeliveryMode == domain.BindingDeliverySummary {
		return ResourceContent{}, ErrPackageNotFound
	}

	versionID := binding.PinnedVersionID
	if versionID == nil || *versionID == uuid.Nil {
		v, verr := s.assets.ResolveVersion(ctx, assetID, versionSpec)
		if verr != nil {
			return ResourceContent{}, ErrPackageNotFound
		}
		versionID = &v.ID
	}
	pkg, err := s.packages.Get(ctx, *versionID)
	if err != nil {
		return ResourceContent{}, ErrPackageNotFound
	}
	// The path MUST match a manifest entry — no traversal, no synthetic paths.
	// package.Parse already enforced path-traversal at import; re-checking
	// here is defence-in-depth against a manifest tampered post-import.
	if !manifestHasPath(pkg.Manifest, resourcePath) {
		return ResourceContent{}, ErrPackageNotFound
	}
	// Stream the archive + locate the entry. exec count stays 0 (Parse only).
	parsed, err := skillpkg.Parse(pkg.StorageKey, openerAdapter{s.opener})
	if err != nil {
		return ResourceContent{}, ErrPackageNotFound
	}
	for _, f := range parsed.Package.Files {
		if f.Path == resourcePath {
			// Symlink entries (no content) are not readable bytes.
			if f.Hash == "" {
				return ResourceContent{}, ErrPackageNotFound
			}
			return ResourceContent{
				Path:        f.Path,
				Hash:        f.Hash,
				Kind:        string(f.Kind),
				Content:     f.Content,
				ContentHash: pkg.ContentHash,
			}, nil
		}
	}
	return ResourceContent{}, ErrPackageNotFound
}

// ResourceContent is one progressive resource read result (§6.2). Content is
// the decompressed file bytes; Hash is the sha256 the manifest recorded (the
// caller can verify integrity). Kind is the manifest's file kind
// (skill_md/script/asset/manifest/other) — metadata only, NEVER an exec hint.
type ResourceContent struct {
	Path        string `json:"path"`
	Hash        string `json:"hash"`
	Kind        string `json:"kind"`
	Content     []byte `json:"-"`
	ContentHash string `json:"content_hash"` // the package roundtrip anchor
}

// SkillListItem is one entry of the skill_list result (§6.3 skill_list /
// §6.2 GET /internal/v1/skills). It is the trimmed, agent-visible projection
// of a skill the agent is bound to: the SKILL.md header (name/description/
// version) + the effective delivery_mode + the resolved version. Raw file
// bytes / manifest are NOT inlined here (skill_read / skill_resources fetch
// them progressively) — the list item is the directory entry, not the
// contents.
//
// Existence never leaks (§8.2): a skill the agent has no allow binding for,
// or whose effective binding is deny, is simply ABSENT from the list — the
// agent cannot tell an unbound skill from one that does not exist.
type SkillListItem struct {
	AssetID       uuid.UUID                 `json:"asset_id"`
	Name          string                    `json:"name"`
	Description   string                    `json:"description,omitempty"`
	Version       string                    `json:"version,omitempty"`
	VersionNo     int64                     `json:"version_no,omitempty"`
	DeliveryMode  domain.BindingDeliveryMode `json:"delivery_mode"`
	FormatID      domain.SkillFormatID      `json:"format_id,omitempty"`
	ContentHash   string                    `json:"content_hash,omitempty"`
}

// List enumerates the skills an agent is bound to in a workspace (§6.3
// skill_list / §6.2 GET /internal/v1/skills). It is the listing counterpart
// of Deliver: it walks the workspace's skill assets and keeps only those the
// agent's effective binding allows (deny / no-binding dropped — no existence
// leak, §8.2). Each kept item carries the SKILL.md header + the resolved
// version + the effective delivery_mode, but no raw bytes (progressive reads
// come via skill_read / skill_resources).
//
// A nil agentID (service_account caller with no agent context) → empty list
// (§11.2 — the internal token alone never authorizes skill discovery). An
// empty result is the normal, leak-safe outcome for an unbound agent; it is
// indistinguishable from a workspace with no skills.
func (s *DeliveryService) List(ctx context.Context, agentID, workspaceID uuid.UUID) ([]SkillListItem, error) {
	if agentID == uuid.Nil {
		return []SkillListItem{}, nil
	}
	assets, err := s.assets.ListSkillsByWorkspace(ctx, workspaceID)
	if err != nil {
		// A listing failure must not leak — return an empty result, never an
		// error to the caller (§8.2 no-leak — the agent cannot infer whether
		// skills exist from a transient repo fault).
		return []SkillListItem{}, nil
	}
	if len(assets) == 0 {
		return []SkillListItem{}, nil
	}
	// Resolve the agent's active bindings once; the in-memory precedence is
	// applied per-asset by resolveEffectiveBinding among these candidates.
	items := make([]SkillListItem, 0, len(assets))
	for _, asset := range assets {
		binding, err := s.resolveEffectiveBinding(ctx, agentID, workspaceID, asset.ID, asset.AssetType)
		if err != nil {
			// No allow binding (or deny wins) → drop the skill. Silence is the
			// leak-safe outcome (the agent cannot tell unbound from absent).
			continue
		}
		// Resolve the concrete version (pinned > latest published) so the list
		// item reports the version the agent would receive. A version that
		// fails to resolve (e.g. no published version yet) drops the skill —
		// surfacing a header with no deliverable version would mislead.
		versionSpec := "latest"
		item := SkillListItem{
			AssetID:      asset.ID,
			Name:         asset.Name,
			Description:  asset.Description,
			DeliveryMode: binding.DeliveryMode,
		}
		versionID := binding.PinnedVersionID
		if versionID != nil && *versionID != uuid.Nil {
			versionSpec = versionID.String()
		}
		v, verr := s.assets.ResolveVersion(ctx, asset.ID, versionSpec)
		if verr != nil {
			continue
		}
		item.VersionNo = v.VersionNo
		// The SKILL.md frontmatter (name/version) + format_id + content_hash
		// come from the stored package, looked up by the resolved version.
		if pkg, perr := s.packages.Get(ctx, v.ID); perr == nil {
			item.ContentHash = pkg.ContentHash
			item.FormatID = pkg.FormatID
			if fm := pkg.OriginalFrontmatter; fm != nil {
				if n, ok := fm["name"].(string); ok && n != "" {
					item.Name = n
				}
				if d, ok := fm["description"].(string); ok && d != "" {
					item.Description = d
				}
				if ver, ok := fm["version"].(string); ok && ver != "" {
					item.Version = ver
				}
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// resolveEffectiveBinding picks the binding governing delivery of one asset to
// one agent (§5.3). Precedence:
//  1. Explicit deny beats explicit allow (AC-7: 显式拒绝 > 显式允许).
//  2. Among the same effect, higher priority wins.
//  3. Among the same effect + priority, narrower scope wins: asset >
//     asset_type > workspace (a deny on the asset beats a deny on the type).
// A binding matches the asset when:
//   - scope=asset      AND asset_id = the asset
//   - scope=asset_type AND asset_type = the asset's type
//   - scope=workspace  (covers every asset in the workspace)
// No matching allow binding → ErrPackageNotFound (the agent may not access this
// skill at all; no existence leak). A matching deny (even alongside an allow)
// → ErrPackageNotFound (deny wins; surfaced as not-found, no leak).
func (s *DeliveryService) resolveEffectiveBinding(ctx context.Context, agentID, workspaceID, assetID uuid.UUID, assetType domain.AssetType) (domain.AgentBinding, error) {
	candidates, err := s.bindings.ActiveForAgent(ctx, agentID, workspaceID)
	if err != nil {
		return domain.AgentBinding{}, ErrPackageNotFound
	}
	// Filter to bindings whose scope matches this asset.
	matching := make([]domain.AgentBinding, 0, len(candidates))
	for _, b := range candidates {
		if bindingMatchesAsset(b, assetID, assetType) {
			matching = append(matching, b)
		}
	}
	if len(matching) == 0 {
		return domain.AgentBinding{}, ErrPackageNotFound
	}
	// §5.3 precedence: sort so the "winner" is first. A deny always wins over an
	// allow; within the same effect, higher priority then narrower scope. The
	// sort is stable on (effect, priority, scope-rank) so the head is decisive.
	sort.SliceStable(matching, func(i, j int) bool {
		return bindingPrecedes(matching[i], matching[j])
	})
	winner := matching[0]
	if winner.Effect == domain.BindingDeny {
		// Deny wins → not deliverable. Surface not-found (no leak — the caller
		// cannot distinguish a denied skill from a missing one).
		return domain.AgentBinding{}, ErrPackageNotFound
	}
	return winner, nil
}

// bindingMatchesAsset reports whether a binding's scope covers the asset.
func bindingMatchesAsset(b domain.AgentBinding, assetID uuid.UUID, assetType domain.AssetType) bool {
	switch b.ScopeKind {
	case domain.BindingScopeAsset:
		return b.AssetID != nil && *b.AssetID == assetID
	case domain.BindingScopeAssetType:
		return b.AssetType != nil && *b.AssetType == assetType
	case domain.BindingScopeWorkspace:
		return true
	}
	return false
}

// scopeRank orders scope narrowness: asset (0) < asset_type (1) < workspace (2).
// Lower = narrower = wins ties in the precedence sort.
func scopeRank(k domain.BindingScopeKind) int {
	switch k {
	case domain.BindingScopeAsset:
		return 0
	case domain.BindingScopeAssetType:
		return 1
	case domain.BindingScopeWorkspace:
		return 2
	}
	return 3
}

// bindingPrecedes reports whether a should sort before b in the §5.3 precedence.
// Deny < allow (deny wins); within the same effect, higher priority first;
// within the same effect + priority, narrower scope first.
func bindingPrecedes(a, b domain.AgentBinding) bool {
	aDeny := a.Effect == domain.BindingDeny
	bDeny := b.Effect == domain.BindingDeny
	if aDeny != bDeny {
		return aDeny // deny sorts first
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority // higher priority first
	}
	return scopeRank(a.ScopeKind) < scopeRank(b.ScopeKind) // narrower first
}

// manifestHasPath reports whether path is a manifest entry (defence-in-depth
// against a path-traversal or synthetic path on the progressive-read path).
func manifestHasPath(m domain.SkillManifest, path string) bool {
	for _, f := range m.Files {
		if f.Path == path {
			return true
		}
	}
	return false
}

// openerAdapter (defined in service.go) is shared across the skill service's
// import/validate/export paths and this delivery path — same package, one type.

