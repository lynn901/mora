package authz

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// AssetLocator resolves asset targets into [asset, workspace] chains
// (design-docs/13 §3.3). It reads knowledge_assets.workspace_id via AssetRepo.
// A non-existent asset returns ErrTargetNotFound so existence never leaks.
type AssetLocator struct {
	assets AssetRepo
}

// NewAssetLocator builds an AssetLocator over an AssetRepo.
func NewAssetLocator(assets AssetRepo) *AssetLocator { return &AssetLocator{assets: assets} }

func (l *AssetLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
	if t != domain.TargetAsset {
		return Location{}, errors.New("asset locator: wrong target type")
	}
	a, err := l.assets.Get(ctx, id)
	if err != nil {
		// Non-existent / unreadable asset: indistinguishable from not-found so
		// existence is never leaked (不变量: 存在性不泄露).
		return Location{}, ErrTargetNotFound
	}
	return Location{
		WorkspaceID: a.WorkspaceID,
		Chain: []Node{
			{Type: domain.TargetAsset, ID: id},
			{Type: domain.TargetWorkspace, ID: a.WorkspaceID},
		},
	}, nil
}

// AgentLocator resolves agent targets into [agent, workspace] chains
// (design-docs/13 §3.3). It reads agents.workspace_id via AgentRepo.
type AgentLocator struct {
	agents AgentRepo
}

// NewAgentLocator builds an AgentLocator over an AgentRepo.
func NewAgentLocator(agents AgentRepo) *AgentLocator { return &AgentLocator{agents: agents} }

func (l *AgentLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
	if t != domain.TargetAgent {
		return Location{}, errors.New("agent locator: wrong target type")
	}
	a, err := l.agents.Get(ctx, id)
	if err != nil {
		return Location{}, ErrTargetNotFound
	}
	return Location{
		WorkspaceID: a.WorkspaceID,
		Chain: []Node{
			{Type: domain.TargetAgent, ID: id},
			{Type: domain.TargetWorkspace, ID: a.WorkspaceID},
		},
	}, nil
}

// EvidenceLocator is a Phase 0 placeholder for evidence targets (§3.3).
// The memory_evidence table arrives in Phase 4; until then Locate returns
// ErrTargetNotFound so evidence is never resolvable (and never leaks).
type EvidenceLocator struct{}

func NewEvidenceLocator() *EvidenceLocator { return &EvidenceLocator{} }

func (l *EvidenceLocator) Locate(_ context.Context, _ TargetType, _ uuid.UUID) (Location, error) {
	return Location{}, ErrTargetNotFound
}

// Compile-time checks.
var (
	_ ResourceLocator = (*AssetLocator)(nil)
	_ ResourceLocator = (*AgentLocator)(nil)
	_ ResourceLocator = (*EvidenceLocator)(nil)
)
