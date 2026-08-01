package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
)

func d(id, parent, name string, order int) domain.Directory {
	d := domain.Directory{ID: uuid.MustParse(id), Name: name, SortOrder: order}
	if parent != "" {
		p := uuid.MustParse(parent)
		d.ParentID = &p
	}
	return d
}

func TestBuildTree_Nested(t *testing.T) {
	dirs := []domain.Directory{
		d("00000000-0000-0000-0000-000000000001", "", "root-a", 1),
		d("00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000001", "child-a", 2),
		d("00000000-0000-0000-0000-000000000003", "00000000-0000-0000-0000-000000000001", "child-b", 1),
		d("00000000-0000-0000-0000-000000000004", "00000000-0000-0000-0000-000000000002", "grandchild", 1),
	}
	tree := BuildTree(dirs, nil)
	assert.Len(t, tree, 1)
	assert.Equal(t, "root-a", tree[0].Name)
	assert.Len(t, tree[0].Children, 2)
	// child-b (order 1) before child-a (order 2)
	assert.Equal(t, "child-b", tree[0].Children[0].Name)
	assert.Equal(t, "child-a", tree[0].Children[1].Name)
	assert.Len(t, tree[0].Children[1].Children, 1)
	assert.Equal(t, "grandchild", tree[0].Children[1].Children[0].Name)
}

func TestBuildTree_SortByName(t *testing.T) {
	dirs := []domain.Directory{
		d("00000000-0000-0000-0000-000000000011", "", "zebra", 0),
		d("00000000-0000-0000-0000-000000000012", "", "alpha", 0),
		d("00000000-0000-0000-0000-000000000013", "", "mango", 0),
	}
	tree := BuildTree(dirs, nil)
	assert.Equal(t, "alpha", tree[0].Name)
	assert.Equal(t, "mango", tree[1].Name)
	assert.Equal(t, "zebra", tree[2].Name)
}

func TestPathLabel(t *testing.T) {
	assert.Equal(t, "api", PathLabel("API 设计规范")) // CJK collapses+trims
	assert.Equal(t, "hello_world", PathLabel("Hello World!"))
	assert.Equal(t, "node", PathLabel("。。。"))
}

func TestChildPath(t *testing.T) {
	assert.Equal(t, "root", ChildPath("", "Root"))
	assert.Equal(t, "root.api", ChildPath("root", "API 设计规范"))
}
