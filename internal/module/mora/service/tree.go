package service

// Package service contains mora domain business logic that is pure (no I/O):
// directory tree assembly, document service orchestration interfaces, and
// version diffing. Persistence lives in internal/infra/postgres.

import (
	"sort"

	"github.com/lynn901/mora/internal/domain"
)

// BuildTree assembles a flat list of directories into a nested tree rooted at
// the given parent (nil = workspace root). Implements infinite-level nesting
// (PRD F1.3) with path/sort ordering (AC-4).
func BuildTree(dirs []domain.Directory, parentID *domain.UUID) []*domain.Directory {
	byParent := make(map[string][]*domain.Directory)
	for i := range dirs {
		d := &dirs[i]
		key := "root"
		if d.ParentID != nil {
			key = d.ParentID.String()
		}
		byParent[key] = append(byParent[key], d)
	}
	return buildChildren(byParent, parentID)
}

func buildChildren(byParent map[string][]*domain.Directory, parentID *domain.UUID) []*domain.Directory {
	key := "root"
	if parentID != nil {
		key = parentID.String()
	}
	children := byParent[key]
	sort.Slice(children, func(i, j int) bool {
		if children[i].SortOrder != children[j].SortOrder {
			return children[i].SortOrder < children[j].SortOrder
		}
		return children[i].Name < children[j].Name
	})
	for _, c := range children {
		c.Children = buildChildren(byParent, &c.ID)
	}
	return children
}

// PathLabel builds an ltree-safe path label from a name. ltree labels must
// match [a-zA-Z0-9_]+; non-ASCII (e.g. CJK) collapses to a single '_' and
// trailing underscores are trimmed.
func PathLabel(name string) string {
	var b []byte
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b = append(b, byte(r))
		case r >= 'A' && r <= 'Z':
			b = append(b, byte(r+32))
		case r >= '0' && r <= '9':
			b = append(b, byte(r))
		case r == '_':
			b = append(b, '_')
		default:
			if len(b) > 0 && b[len(b)-1] != '_' {
				b = append(b, '_')
			}
		}
	}
	// trim trailing underscores
	for len(b) > 0 && b[len(b)-1] == '_' {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return "node"
	}
	return string(b)
}

// ChildPath returns the ltree path for a child given its parent path and name.
func ChildPath(parentPath, name string) string {
	label := PathLabel(name)
	if parentPath == "" {
		return label
	}
	return parentPath + "." + label
}
