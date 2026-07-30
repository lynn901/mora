package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wiki/wiki-backend/internal/domain"
)

func mkText(id, text string) domain.Block {
	return domain.Block{ID: id, Type: domain.BlockText, Text: text}
}

func TestDiff_NoChanges(t *testing.T) {
	from := []domain.Block{mkText("b1", "hello")}
	to := []domain.Block{mkText("b1", "hello")}
	entries := Diff(from, to)
	assert.Empty(t, entries)
	assert.Equal(t, "no changes", Summary(entries))
}

func TestDiff_AddedBlock(t *testing.T) {
	from := []domain.Block{mkText("b1", "a")}
	to := []domain.Block{mkText("b1", "a"), mkText("b2", "b")}
	entries := Diff(from, to)
	requireLen(t, entries, 1)
	assert.Equal(t, "added", entries[0].Type)
	assert.Equal(t, "b2", entries[0].BlockID)
	assert.Contains(t, Summary(entries), "1 addition")
}

func TestDiff_RemovedBlock(t *testing.T) {
	from := []domain.Block{mkText("b1", "a"), mkText("b2", "b")}
	to := []domain.Block{mkText("b1", "a")}
	entries := Diff(from, to)
	requireLen(t, entries, 1)
	assert.Equal(t, "removed", entries[0].Type)
	assert.Contains(t, Summary(entries), "1 removal")
}

func TestDiff_ModifiedBlock(t *testing.T) {
	from := []domain.Block{mkText("b1", "old")}
	to := []domain.Block{mkText("b1", "new")}
	entries := Diff(from, to)
	requireLen(t, entries, 1)
	assert.Equal(t, "modified", entries[0].Type)
	assert.Equal(t, "old", entries[0].From.Text)
	assert.Equal(t, "new", entries[0].To.Text)
	assert.Contains(t, Summary(entries), "1 modification")
}

func TestDiff_MixedChanges(t *testing.T) {
	from := []domain.Block{mkText("b1", "keep"), mkText("b2", "remove"), mkText("b3", "change")}
	to := []domain.Block{mkText("b1", "keep"), mkText("b3", "changed"), mkText("b4", "add")}
	entries := Diff(from, to)
	requireLen(t, entries, 3)
	assert.Contains(t, Summary(entries), "addition")
	assert.Contains(t, Summary(entries), "removal")
	assert.Contains(t, Summary(entries), "modification")
}

func TestNextVersionNo(t *testing.T) {
	assert.Equal(t, 1, NextVersionNo(0))
	assert.Equal(t, 1, NextVersionNo(-1))
	assert.Equal(t, 6, NextVersionNo(5))
}

func requireLen(t *testing.T, s []DiffEntry, n int) {
	t.Helper()
	if len(s) != n {
		t.Fatalf("expected %d entries, got %d: %+v", n, len(s), s)
	}
}
