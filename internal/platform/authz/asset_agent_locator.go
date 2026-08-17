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

// EvidenceLocator resolves TargetEvidence → [evidence, source_asset?, workspace]
// (design-docs/18 §4.4, decision D2). Evidence ACL is independent of Memory
// publish: publishing a Memory never writes a permissions(target_type='evidence')
// row (附录 A 不变量 8) — this locator only resolves where an evidence record
// lives so the decision pipeline can consult the owner + source-asset current
// ACL for the §4.3 read chain.
//
// A missing or deleted evidence returns ErrTargetNotFound so existence is
// never leaked (§9.3). A source_asset_id that is nil (session/message/
// tool_call evidence) omits the source-asset node; a source_asset that was
// deleted does NOT block resolution — the locator returns evidence + workspace
// nodes, and the §4.3 chain handles evidence_missing by denying plaintext
// expansion (§4.3 "来源删除/不可定位 → 原文默认不可展开").
type EvidenceLocator struct {
	evidence EvidenceRepo
}

// NewEvidenceLocator builds an EvidenceLocator over an authz EvidenceRepo.
// The repo is the authz-side narrow port, not the module/memory full CRUD
// repo — same narrow-port split as AssetLocator/SourceLocator.
func NewEvidenceLocator(evidence EvidenceRepo) *EvidenceLocator {
	return &EvidenceLocator{evidence: evidence}
}

func (l *EvidenceLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
	if t != domain.TargetEvidence {
		return Location{}, errors.New("evidence locator: wrong target type")
	}
	e, err := l.evidence.Get(ctx, id)
	if err != nil {
		// Non-existent / deleted evidence: indistinguishable from not-found so
		// existence is never leaked (不变量: 存在性不泄露, §9.3).
		return Location{}, ErrTargetNotFound
	}
	chain := []Node{{Type: domain.TargetEvidence, ID: id}}
	// Include the source-asset node when the evidence carries one. The asset's
	// CURRENT ACL (not the captured_authz_revision snapshot) is the second check
	// in the §4.3 read chain; the locator surfaces the asset id so the decision
	// pipeline can resolve it via the AssetLocator. A nil asset (no source) or a
	// deleted asset (resolved-but-missing by AssetLocator) does not fail here —
	// the read chain above treats it as evidence_missing.
	if e.SourceAssetID != nil {
		chain = append(chain, Node{Type: domain.TargetAsset, ID: *e.SourceAssetID})
	}
	chain = append(chain, Node{Type: domain.TargetWorkspace, ID: e.WorkspaceID})
	return Location{WorkspaceID: e.WorkspaceID, Chain: chain}, nil
}

// SourceLocator resolves source targets into [source, workspace] chains
// (design-docs/14 §3.3 / §8.2). It reads knowledge_sources.workspace_id +
// enabled via the authz SourceRepo. A missing OR disabled source returns
// ErrTargetNotFound so existence never leaks — a disabled source is, for
// authorization purposes, invisible (§4.4 DELETE = soft-disable, and a
// disabled source must not appear in any later decision).
type SourceLocator struct {
	sources SourceRepo
}

// NewSourceLocator builds a SourceLocator over an authz SourceRepo.
func NewSourceLocator(sources SourceRepo) *SourceLocator {
	return &SourceLocator{sources: sources}
}

func (l *SourceLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
	if t != domain.TargetSource {
		return Location{}, errors.New("source locator: wrong target type")
	}
	s, err := l.sources.Get(ctx, id)
	if err != nil {
		// Non-existent / unreadable / disabled source: indistinguishable from
		// not-found so existence is never leaked (不变量: 存在性不泄露).
		return Location{}, ErrTargetNotFound
	}
	if !s.Enabled {
		// A disabled source is invisible to authorization — surface as
		// not-found, not as "found but denied", so callers can't infer it.
		return Location{}, ErrTargetNotFound
	}
	return Location{
		WorkspaceID: s.WorkspaceID,
		Chain: []Node{
			{Type: domain.TargetSource, ID: id},
			{Type: domain.TargetWorkspace, ID: s.WorkspaceID},
		},
	}, nil
}

// ReviewLocator resolves review targets into [review, workspace] chains
// (design-docs/14 §3.3 / §8.2). It reads review_requests.workspace_id via the
// authz ReviewRepo. A missing request returns ErrTargetNotFound so existence
// never leaks.
type ReviewLocator struct {
	reviews ReviewRepo
}

// NewReviewLocator builds a ReviewLocator over an authz ReviewRepo.
func NewReviewLocator(reviews ReviewRepo) *ReviewLocator {
	return &ReviewLocator{reviews: reviews}
}

func (l *ReviewLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
	if t != domain.TargetReview {
		return Location{}, errors.New("review locator: wrong target type")
	}
	r, err := l.reviews.Get(ctx, id)
	if err != nil {
		return Location{}, ErrTargetNotFound
	}
	return Location{
		WorkspaceID: r.WorkspaceID,
		Chain: []Node{
			{Type: domain.TargetReview, ID: id},
			{Type: domain.TargetWorkspace, ID: r.WorkspaceID},
		},
	}, nil
}

// Compile-time checks.
var (
	_ ResourceLocator = (*AssetLocator)(nil)
	_ ResourceLocator = (*AgentLocator)(nil)
	_ ResourceLocator = (*EvidenceLocator)(nil)
	_ ResourceLocator = (*SourceLocator)(nil)
	_ ResourceLocator = (*ReviewLocator)(nil)
)
