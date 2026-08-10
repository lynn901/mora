// Package authz is the unified authorization layer (design-docs/12 §3.1, §5).
//
// It owns the ResourceLocator port, the decision pipeline, and (in later
// PRs) the authz.Service that composes the legacy rbac.Engine as one strategy.
// The legacy rbac.Engine keeps its Check/VisibleDocuments contract unchanged;
// its internal locate/targetChain is refactored to delegate to an injected
// ResourceLocator (defaulting to docLocator), so engine_test.go stays green.
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// TargetType is an alias of domain.TargetType so callers can reference the
// locator port without importing domain, while staying wire-compatible.
// Keeping domain as the single source of truth avoids a parallel type system
// (§5.4: "扩展现有类型，不另建平行 ACL").
type TargetType = domain.TargetType

// ResourceLocator resolves a target (type+id) into an authoritative location
// within a workspace: the workspace it belongs to and an ordered chain of
// ancestor nodes from most-specific to least-specific, used by the decision
// pipeline for inheritance resolution.
//
// Implementations MUST be side-effect free (read-only) and MUST NOT leak the
// existence of a target the caller cannot see: resolving a non-existent or
// non-visible target returns ErrTargetNotFound, indistinguishable from "no
// permission" to the caller (存在性不泄露).
type ResourceLocator interface {
	Locate(ctx context.Context, targetType TargetType, targetID uuid.UUID) (Location, error)
}

// Location is the resolved position of a target.
type Location struct {
	WorkspaceID uuid.UUID
	// Chain is the evaluation order, most-specific first. Each node carries
	// a target type+id that grants may attach to. For a document this is
	// [document, directory, ..., workspace-root, workspace]; for a workspace
	// it is [workspace]; for an asset it is [asset, workspace].
	Chain []Node
}

// Node is a single level in a target's ancestor chain.
type Node struct {
	Type TargetType
	ID   uuid.UUID
}

// ErrTargetNotFound is returned when a target cannot be resolved or is not
// visible to the caller. It is indistinguishable from a permission denial so
// existence of non-visible resources is never leaked.
var ErrTargetNotFound = errors.New("authz: target not found or not visible")

// CompositeLocator routes Locate by target type to a registered child locator.
// Adding a new target type = register a new child locator; no switch grows.
type CompositeLocator struct {
	children map[TargetType]ResourceLocator
}

// NewCompositeLocator builds a CompositeLocator from the given child locators.
// Later registrations for the same target type override earlier ones.
func NewCompositeLocator(children ...struct {
	Type TargetType
	Loc  ResourceLocator
}) *CompositeLocator {
	c := &CompositeLocator{children: make(map[TargetType]ResourceLocator, len(children))}
	for _, ch := range children {
		c.children[ch.Type] = ch.Loc
	}
	return c
}

// Register associates a child locator with a target type. Later calls for the
// same type replace the previous locator.
func (c *CompositeLocator) Register(t TargetType, loc ResourceLocator) {
	c.children[t] = loc
}

// Locate routes to the registered child for targetType.
func (c *CompositeLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
	child, ok := c.children[t]
	if !ok {
		return Location{}, fmt.Errorf("%w: unsupported target type %s", ErrTargetNotFound, t)
	}
	return child.Locate(ctx, t, id)
}
