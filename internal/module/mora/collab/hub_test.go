package collab

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestHub_AdmitEditors(t *testing.T) {
	h := NewHub(2)
	doc := uuid.New()
	c1 := NewClient(uuid.New(), "alice", doc)
	c2 := NewClient(uuid.New(), "bob", doc)

	r1, _ := h.Register(c1)
	assert.True(t, r1.Admitted)
	assert.False(t, r1.ReadOnly)

	r2, _ := h.Register(c2)
	assert.True(t, r2.Admitted)
	assert.False(t, r2.ReadOnly)
	assert.Equal(t, 2, h.EditorCount(doc))
}

func TestHub_DegradeAtLimit(t *testing.T) {
	h := NewHub(2)
	doc := uuid.New()
	h.Register(NewClient(uuid.New(), "a", doc))
	h.Register(NewClient(uuid.New(), "b", doc))

	// third editor must be degraded to read-only
	c3 := NewClient(uuid.New(), "c", doc)
	r3, _ := h.Register(c3)
	assert.True(t, r3.Admitted, "third joiner still admitted")
	assert.True(t, r3.ReadOnly, "third joiner degraded to read-only")
	assert.NotEmpty(t, r3.Reason, "degradation reason provided")
	assert.Equal(t, 2, h.EditorCount(doc), "editor count stays at limit")
}

func TestHub_ReadOnlyJoinerDoesNotConsumeEditorSlot(t *testing.T) {
	h := NewHub(1)
	doc := uuid.New()
	h.Register(NewClient(uuid.New(), "a", doc))

	ro := NewClient(uuid.New(), "viewer", doc)
	ro.ReadOnly = true
	r, _ := h.Register(ro)
	assert.True(t, r.Admitted)
	assert.True(t, r.ReadOnly)
	assert.Equal(t, 1, h.EditorCount(doc), "read-only joiner does not take editor slot")
}

func TestHub_UnregisterFreesSlot(t *testing.T) {
	h := NewHub(1)
	doc := uuid.New()
	c1 := NewClient(uuid.New(), "a", doc)
	h.Register(c1)
	h.Unregister(doc, c1.UserID)
	assert.Equal(t, 0, h.EditorCount(doc))

	// now a new editor can join as editor (not degraded)
	c2 := NewClient(uuid.New(), "b", doc)
	r, _ := h.Register(c2)
	assert.False(t, r.ReadOnly)
}

func TestHub_BroadcastReachesAll(t *testing.T) {
	h := NewHub(10)
	doc := uuid.New()
	c1 := NewClient(uuid.New(), "a", doc)
	c2 := NewClient(uuid.New(), "b", doc)
	h.Register(c1)
	h.Register(c2)

	h.Broadcast(doc, Message{Type: "update"})
	assert.Len(t, c1.Send(), 1)
	assert.Len(t, c2.Send(), 1)
}

func TestHub_Roster(t *testing.T) {
	h := NewHub(10)
	doc := uuid.New()
	h.Register(NewClient(uuid.New(), "a", doc))
	h.Register(NewClient(uuid.New(), "b", doc))
	ro := NewClient(uuid.New(), "v", doc)
	ro.ReadOnly = true
	h.Register(ro)

	roster := h.Roster(doc)
	assert.Len(t, roster, 3)
}
