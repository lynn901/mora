package asset

// readservice.go is the read-side application service for knowledge assets
// (design-docs/14 §4.4 D13: GET /knowledge/assets/:id, /:id/versions,
// /:id/relations, GET /workspaces/:ws/knowledge/assets). It mirrors the
// source service's resource-level RBAC exactly: every method calls
// rbac.Engine.Check before touching a resource, and the CompositeLocator
// resolves an asset to its owning workspace — a missing OR cross-workspace
// asset fails to resolve → ErrAssetNotFound, so a caller outside the asset's
// workspace has no grant and is denied (§8.5 / §10.4 用例 26/27).
//
// Existence never leaks (§8.2): a read denial returns ErrAssetNotFound, the
// SAME sentinel a genuinely missing asset returns, so the handler surfaces
// 404 + 40400 indistinguishable from not-found. Asset reads are read-only,
// so there is no 403/Forbidden path here (a write/governance denial on an
// asset is a separate concern, handled by the lifecycle endpoints elsewhere).

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/audit"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// Sentinel errors. The handler maps these to the §11.4 envelope: a read
// not-found → 404 + 40400 (no existence leak). They MUST stay indistinguishable
// from a permission denial so a caller cannot tell "not found" from "not
// allowed" (§8.2 / §10.4 用例 26/27).
var (
	ErrAssetNotFound = errors.New("asset: not found")
)

// ReadRepo is the read-side asset port (infra/postgres implements it). The
// service owns RBAC; the repo only fetches rows already scoped by the service
// to the caller's workspace. A missing asset returns ErrAssetNotFound so the
// repo and the RBAC denial surface identically (no leak).
type ReadRepo interface {
	// Get loads a single knowledge asset by id. Returns ErrAssetNotFound when
	// the row does not exist. Does NOT leak existence — the service gates the
	// call on an RBAC read check first.
	Get(ctx context.Context, id uuid.UUID) (*domain.KnowledgeAsset, error)
	// List returns a cursor-paginated page of assets in workspaceID, filtered by
	// asset_type/status. The repo MUST scope by workspace_id so a non-member
	// never sees another workspace's assets.
	List(ctx context.Context, q ListQuery) ([]*domain.KnowledgeAsset, string, error)
	// ListVersions returns the version history of an asset (newest-first).
	ListVersions(ctx context.Context, assetID uuid.UUID) ([]*domain.AssetVersion, error)
	// GetVersionByID loads one asset version by id, joined to its asset's
	// workspace id (Phase 3 codegraph build path, 17 §4.1). A missing version
	// returns ErrAssetNotFound (no existence leak — the build handler maps it
	// to a permanent fail-closed job).
	GetVersionByID(ctx context.Context, versionID uuid.UUID) (*domain.AssetVersion, uuid.UUID, error)
	// ListRelations returns the knowledge_relations edges of an asset, optionally
	// filtered by relation_type.
	ListRelations(ctx context.Context, assetID uuid.UUID, relationType string) ([]*domain.KnowledgeRelation, error)
}

// ListQuery is the cursor-paginated asset-list filter (§4.4 GET /assets).
type ListQuery struct {
	WorkspaceID uuid.UUID
	Cursor      string
	PageSize    int
	AssetType   string // empty = all
	Status      string // empty = all
}

// ReadService is the asset read application service. It enforces resource-level
// RBAC via the same rbac.Engine the source service uses; rbac==nil is dev/test
// only and MUST NOT ship in main.go (chain WithAuthz).
type ReadService struct {
	repo  ReadRepo
	rbac  *rbac.Engine // nil = no resource-level authz (dev/test only)
	audit *audit.Logger
}

// NewReadService wires the asset read service. rbac is nil here by design:
// production wiring MUST chain WithAuthz so every method enforces resource-
// level RBAC (§8.5). Mirrors source.NewService.
func NewReadService(repo ReadRepo) *ReadService { return &ReadService{repo: repo} }

// WithAuthz injects the RBAC engine + audit logger and returns the service for
// chaining (same pattern as source.Service.WithAuthz).
func (s *ReadService) WithAuthz(engine *rbac.Engine, logger *audit.Logger) *ReadService {
	s.rbac = engine
	s.audit = logger
	return s
}

// authorize runs an rbac.Engine.Check for an asset target. Asset reads are
// leak=false: a denial (including a missing/cross-workspace asset that the
// locator cannot resolve) returns ErrAssetNotFound so existence never leaks
// (§8.2 / §10.4 用例 26/27). An admin short-circuits to allowed; a nil rbac
// (unit tests only) also allows — production wiring MUST chain WithAuthz.
func (s *ReadService) authorize(ctx context.Context, auth AuthContext, id uuid.UUID) error {
	if s.rbac == nil || auth.IsAdmin {
		return nil
	}
	dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs, domain.TargetAsset, id, domain.ActionRead)
	if err != nil {
		// The locator returns an error only for a MISSING / cross-workspace-
		// UNRESOLVABLE asset (the asset genuinely does not resolve for this
		// caller). Map to not-found so existence never leaks (§10.4 用例 27).
		return ErrAssetNotFound
	}
	if !dec.Allowed {
		// A read denial MUST NOT leak existence — surface as not-found
		// (§10.4 用例 26: no read permission → 404, not 403).
		return ErrAssetNotFound
	}
	return nil
}

// GetAsset loads an asset by id after an RBAC read check. Existence never
// leaks: a missing asset AND a read denial both surface as ErrAssetNotFound
// (§8.2 / §10.4 用例 26/27 cross-workspace → 404, no leak).
func (s *ReadService) GetAsset(ctx context.Context, auth AuthContext, id uuid.UUID) (*domain.KnowledgeAsset, error) {
	if err := s.authorize(ctx, auth, id); err != nil {
		return nil, err
	}
	a, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// ListAssets returns a cursor-paginated page (§4.4 GET /assets). The repo
// already scopes by workspace_id; this gates the call on a workspace read grant
// so a non-member gets an empty/forbidden result rather than a cross-workspace
// listing (§10.4 用例 27).
func (s *ReadService) ListAssets(ctx context.Context, auth AuthContext, q ListQuery) ([]*domain.KnowledgeAsset, string, error) {
	if s.rbac != nil && !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs, domain.TargetWorkspace, q.WorkspaceID, domain.ActionRead)
		if err != nil || !dec.Allowed {
			// A workspace read denial returns an empty result, not a 404 —
			// listing an empty workspace is not an existence leak, and the
			// caller is already a member of the workspace path they named.
			return []*domain.KnowledgeAsset{}, "", nil
		}
	}
	return s.repo.List(ctx, q)
}

// ListVersions returns an asset's version history after an RBAC read check
// (§4.4 GET /assets/:id/versions). Cross-workspace → ErrAssetNotFound (no leak).
func (s *ReadService) ListVersions(ctx context.Context, auth AuthContext, id uuid.UUID) ([]*domain.AssetVersion, error) {
	if err := s.authorize(ctx, auth, id); err != nil {
		return nil, err
	}
	return s.repo.ListVersions(ctx, id)
}

// ListRelations returns an asset's relation edges after an RBAC read check
// (§4.4 GET /assets/:id/relations). Cross-workspace → ErrAssetNotFound (no
// leak, §10.4 用例 27).
func (s *ReadService) ListRelations(ctx context.Context, auth AuthContext, id uuid.UUID, relationType string) ([]*domain.KnowledgeRelation, error) {
	if err := s.authorize(ctx, auth, id); err != nil {
		return nil, err
	}
	return s.repo.ListRelations(ctx, id, relationType)
}
