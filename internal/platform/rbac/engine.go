package rbac

// Package rbac implements the permission decision engine shared by all modules
// (platform/rbac per 02-system-architecture.md §2.1). Decision priority
// (PRD F1.4): explicit deny > explicit allow > inherited > default deny.
//
// The engine is pure logic over an in-memory Grant set sourced from the
// permissions repository. This keeps the engine trivially unit-testable
// without a database, while the repository (internal/infra/postgres) owns
// persistence and tree-walking for inheritance.
//
// Target resolution (locate/targetChain) may be delegated to an injected
// ResourceLocator (design-docs/13 §3.5, D2). When no locator is set the engine
// uses its built-in doc-family resolution, so NewEngine(repo) and
// engine_test.go keep their pre-existing behavior. The locator contract is
// deliberately minimal and local to avoid an import cycle with platform/authz
// (which depends on rbac for the Repository type); platform/authz.DocLocator
// satisfies Locator transparently via target resolution.

import (
	"context"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// Locator resolves a target into its workspace id and most-specific-first
// ancestor chain. It is the minimal contract the engine needs to delegate
// locate/targetChain (design-docs/13 §3.5). platform/authz.ResourceLocator
// implementations satisfy this by returning their Location; the adapter
// platform/authz.AsLocator bridges the two so callers wire a DocLocator once.
type Locator interface {
	// Locate returns the workspace the target belongs to and the chain of
	// ancestor nodes (type+id) ordered most-specific-first. See engine.locate
	// and engine.targetChain for the built-in doc-family behavior.
	Locate(ctx context.Context, targetType domain.TargetType, targetID uuid.UUID) (workspaceID uuid.UUID, chain []LocatorNode, err error)
}

// LocatorNode is a single level in a target's ancestor chain. It mirrors
// platform/authz.Node but lives here so rbac does not import authz (which
// would create an import cycle, since authz imports rbac.Repository).
type LocatorNode struct {
	Type domain.TargetType
	ID   uuid.UUID
}

// Engine decides whether a subject may perform an action on a target.
type Engine struct {
	repo    Repository
	locator Locator
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

	// DocumentsInDirectorySubtree returns the IDs of non-deleted documents in a
	// directory and all its descendants. Used by VisibleDocuments to expand a
	// directory-level read grant into the visible document set.
	DocumentsInDirectorySubtree(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error)
}

// NewEngine constructs an engine backed by repo with built-in doc-family
// target resolution. Behavior is identical to pre-delegation; existing tests
// and call sites are unaffected.
func NewEngine(repo Repository) *Engine { return &Engine{repo: repo} }

// SetLocator injects a ResourceLocator (design-docs/13 §3.5). When set, the
// engine delegates locate/targetChain to it; when nil the built-in doc-family
// resolution is used. This is the seam by which CompositeLocator is wired
// without changing Check/VisibleDocuments signatures or behavior for the
// doc path.
func (e *Engine) SetLocator(l Locator) *Engine { e.locator = l; return e }

// Decision captures the outcome of a permission check.
type Decision struct {
	Allowed bool
	Reason  string
}

// Check decides whether subject may perform action on target.
// subject is the user identity; groupIDs are the user's group memberships.
func (e *Engine) Check(ctx context.Context, subject uuid.UUID, groupIDs []uuid.UUID, targetType domain.TargetType, targetID uuid.UUID, action domain.Action) (Decision, error) {
	workspaceID, chain, err := e.resolveTarget(ctx, targetType, targetID)
	if err != nil {
		return Decision{}, err
	}

	grants, err := e.repo.GrantsFor(ctx, subject, groupIDs, workspaceID)
	if err != nil {
		return Decision{}, err
	}

	return decide(grants, chain, action), nil
}

// resolveTarget returns the workspace and most-specific-first ancestor chain
// for a target. When a Locator is injected it delegates to the locator
// (design-docs/13 §3.5); otherwise it falls back to the built-in doc-family
// locate+targetChain, preserving pre-delegation behavior exactly.
func (e *Engine) resolveTarget(ctx context.Context, t domain.TargetType, id uuid.UUID) (uuid.UUID, []node, error) {
	if e.locator != nil {
		wsID, lnodes, err := e.locator.Locate(ctx, t, id)
		if err != nil {
			return uuid.Nil, nil, err
		}
		nodes := make([]node, len(lnodes))
		for i, n := range lnodes {
			nodes[i] = node{typ: n.Type, id: n.ID}
		}
		return wsID, nodes, nil
	}
	// Built-in doc-family path (pre-delegation).
	workspaceID, directoryID, err := e.locate(ctx, t, id)
	if err != nil {
		return uuid.Nil, nil, err
	}
	chain, err := e.targetChain(ctx, t, id, workspaceID, directoryID)
	if err != nil {
		return uuid.Nil, nil, err
	}
	return workspaceID, chain, nil
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
	// First pass: collect explicit deny targets so they can be removed from the
	// visible set (deny > allow > inherited > default-deny, PRD F1.4).
	deniedDocs := make(map[uuid.UUID]bool)
	for _, g := range grants {
		if g.Effect == domain.EffectDeny && hasAction(g.Actions, domain.ActionRead) {
			switch g.TargetType {
			case domain.TargetWorkspace:
				// Workspace-wide deny: nothing visible.
				return map[uuid.UUID]bool{}, nil
			case domain.TargetDirectory:
				docIDs, err := e.repo.DocumentsInDirectorySubtree(ctx, g.TargetID)
				if err != nil {
					return nil, err
				}
				for _, id := range docIDs {
					deniedDocs[id] = true
				}
			case domain.TargetDocument:
				deniedDocs[g.TargetID] = true
			}
		}
	}
	// Second pass: add allow-granted docs, minus denied.
	for _, g := range grants {
		if g.Effect == domain.EffectAllow && hasAction(g.Actions, domain.ActionRead) {
			switch g.TargetType {
			case domain.TargetWorkspace:
				// Workspace-level read grants visibility to all docs in workspace.
				// Caller should mark all workspace docs visible; encoded as nil-key sentinel.
				visible[uuid.Nil] = true
				// Apply denies even under workspace-wide allow.
				for id := range deniedDocs {
					visible[id] = false
				}
				return visible, nil
			case domain.TargetDirectory:
				// Directory-level read (subtree inheritance) → expand to docs.
				docIDs, err := e.repo.DocumentsInDirectorySubtree(ctx, g.TargetID)
				if err != nil {
					return nil, err
				}
				for _, id := range docIDs {
					if !deniedDocs[id] {
						visible[id] = true
					}
				}
			case domain.TargetDocument:
				if !deniedDocs[g.TargetID] {
					visible[g.TargetID] = true
				}
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
