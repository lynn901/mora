package rbac

// Package rbac implements the permission decision engine shared by all modules
// (platform/rbac per 02-system-architecture.md §2.1). Decision priority
// (PRD F1.4): explicit deny > explicit allow > inherited > default deny.
//
// The engine is pure logic over an in-memory Grant set sourced from the
// permissions repository. This keeps the engine trivially unit-testable
// without a database, while the repository (internal/infra/postgres) owns
// persistence and tree-walking for inheritance.

import (
	"context"

	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
)

// Engine decides whether a subject may perform an action on a target.
type Engine struct {
	repo Repository
}

// Repository is the persistence interface the engine depends on.
// Implementations load grants (resolved role->actions) for a subject and
// the ancestor chain of a target, enabling inheritance resolution.
type Repository interface {
	// GrantsFor returns all grants affecting subjectID (as user or via groups)
	// within the given workspace. Each grant carries the actions its role grants.
	GrantsFor(ctx context.Context, subjectID uuid.UUID, groupIDs []uuid.UUID, workspaceID uuid.UUID) ([]domain.Grant, error)

	// DirectoryAncestors returns the directory IDs from workspace-root down to
	// the given directory (inclusive), ordered root-first. Used to resolve
	// inherited directory/workspace permissions.
	DirectoryAncestors(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error)

	// DocumentLocation returns the workspace_id and directory_id of a document.
	DocumentLocation(ctx context.Context, documentID uuid.UUID) (workspaceID, directoryID uuid.UUID, err error)
}

// NewEngine constructs an engine backed by repo.
func NewEngine(repo Repository) *Engine { return &Engine{repo: repo} }

// Decision captures the outcome of a permission check.
type Decision struct {
	Allowed bool
	Reason  string
}

// Check decides whether subject may perform action on target.
// subject is the user identity; groupIDs are the user's group memberships.
func (e *Engine) Check(ctx context.Context, subject uuid.UUID, groupIDs []uuid.UUID, targetType domain.TargetType, targetID uuid.UUID, action domain.Action) (Decision, error) {
	workspaceID, directoryID, err := e.locate(ctx, targetType, targetID)
	if err != nil {
		return Decision{}, err
	}

	grants, err := e.repo.GrantsFor(ctx, subject, groupIDs, workspaceID)
	if err != nil {
		return Decision{}, err
	}

	// Build the ancestor chain (root-first) of IDs relevant to this target,
	// including the target itself as the most-specific node.
	chain, err := e.targetChain(ctx, targetType, targetID, workspaceID, directoryID)
	if err != nil {
		return Decision{}, err
	}

	return decide(grants, chain, action), nil
}

// VisibleDocuments filters a set of document IDs down to those the subject
// may read. Used by search/listing to enforce RBAC at the query layer
// (存在性不泄露 — existence of non-visible docs is never revealed).
func (e *Engine) VisibleDocuments(ctx context.Context, subject uuid.UUID, groupIDs []uuid.UUID, workspaceID uuid.UUID) (map[uuid.UUID]bool, error) {
	grants, err := e.repo.GrantsFor(ctx, subject, groupIDs, workspaceID)
	if err != nil {
		return nil, err
	}
	visible := make(map[uuid.UUID]bool)
	for _, g := range grants {
		if g.Effect == domain.EffectAllow && hasAction(g.Actions, domain.ActionRead) {
			switch g.TargetType {
			case domain.TargetWorkspace:
				// Workspace-level read grants visibility to all docs in workspace.
				// Caller should mark all workspace docs visible; encoded as nil-key sentinel.
				visible[uuid.Nil] = true
				return visible, nil
			case domain.TargetDocument:
				visible[g.TargetID] = true
			}
		}
	}
	return visible, nil
}

// locate resolves the workspace and directory for a target of any type.
func (e *Engine) locate(ctx context.Context, t domain.TargetType, id uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	switch t {
	case domain.TargetWorkspace:
		return id, uuid.Nil, nil
	case domain.TargetDirectory:
		anc, err := e.repo.DirectoryAncestors(ctx, id)
		if err != nil || len(anc) == 0 {
			return uuid.Nil, uuid.Nil, err
		}
		// The first ancestor is the workspace-root directory; workspace id is
		// resolved by the repository which knows the mapping. For the engine
		// we only need workspace for grant scoping; repo returns workspace.
		return anc[0], id, nil
	case domain.TargetDocument:
		return e.repo.DocumentLocation(ctx, id)
	}
	return uuid.Nil, uuid.Nil, nil
}

// targetChain builds the evaluation order (most-specific first).
func (e *Engine) targetChain(ctx context.Context, t domain.TargetType, id, workspaceID, directoryID uuid.UUID) ([]node, error) {
	var nodes []node
	switch t {
	case domain.TargetDocument:
		nodes = append(nodes, node{typ: domain.TargetDocument, id: id})
		if directoryID != uuid.Nil {
			anc, err := e.repo.DirectoryAncestors(ctx, directoryID)
			if err != nil {
				return nil, err
			}
			for i := len(anc) - 1; i >= 0; i-- {
				nodes = append(nodes, node{typ: domain.TargetDirectory, id: anc[i]})
			}
		}
		nodes = append(nodes, node{typ: domain.TargetWorkspace, id: workspaceID})
	case domain.TargetDirectory:
		anc, err := e.repo.DirectoryAncestors(ctx, id)
		if err != nil {
			return nil, err
		}
		for i := len(anc) - 1; i >= 0; i-- {
			nodes = append(nodes, node{typ: domain.TargetDirectory, id: anc[i]})
		}
		nodes = append(nodes, node{typ: domain.TargetWorkspace, id: workspaceID})
	case domain.TargetWorkspace:
		nodes = append(nodes, node{typ: domain.TargetWorkspace, id: workspaceID})
	}
	return nodes, nil
}

type node struct {
	typ domain.TargetType
	id  uuid.UUID
}

// decide applies the priority: explicit deny > explicit allow > inherited > default deny.
// chain is ordered most-specific-first. The first matching grant at the most
// specific level wins: a deny at any level blocks; an allow at a more specific
// level grants; absence falls through to less specific levels.
func decide(grants []domain.Grant, chain []node, action domain.Action) Decision {
	for _, n := range chain {
		deny, allow := false, false
		for _, g := range grants {
			if g.TargetType != n.typ || g.TargetID != n.id {
				continue
			}
			if !hasAction(g.Actions, action) {
				continue
			}
			if g.Effect == domain.EffectDeny {
				deny = true
			} else if g.Effect == domain.EffectAllow {
				allow = true
			}
		}
		// At this level: explicit deny wins over allow.
		if deny {
			return Decision{Allowed: false, Reason: "explicit deny"}
		}
		if allow {
			return Decision{Allowed: true, Reason: "explicit allow"}
		}
		// No decision at this level → fall through to less specific (inherited).
	}
	return Decision{Allowed: false, Reason: "default deny"}
}

func hasAction(actions []domain.Action, want domain.Action) bool {
	for _, a := range actions {
		if a == want || a == domain.ActionAdmin {
			return true // admin implies read+write
		}
		if want == domain.ActionRead && a == domain.ActionWrite {
			return true // write implies read
		}
	}
	return false
}
