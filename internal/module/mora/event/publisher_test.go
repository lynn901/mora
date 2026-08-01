package event

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/module/rag"
	"github.com/stretchr/testify/assert"
)

// fakeQueue captures the DocEvent published through it. Only Publish is real;
// the rest of rag.EventQueue is stubbed (not exercised by QueuePublisher).
type fakeQueue struct {
	got domain.DocEvent
	id  string
}

func (f *fakeQueue) Publish(_ context.Context, ev domain.DocEvent) (string, error) {
	f.got = ev
	return f.id, nil
}
func (f *fakeQueue) ReadGroup(context.Context, string, int64, time.Duration) ([]rag.QueueMessage, error) {
	return nil, nil
}
func (f *fakeQueue) Ack(context.Context, rag.QueueMessage) error                      { return nil }
func (f *fakeQueue) MoveToDeadLetter(context.Context, rag.QueueMessage, string) error { return nil }
func (f *fakeQueue) Claim(context.Context, string, time.Duration, int64) ([]rag.QueueMessage, error) {
	return nil, nil
}

// Compile-time check: fakeQueue satisfies rag.EventQueue.
var _ rag.EventQueue = (*fakeQueue)(nil)

func TestQueuePublisher_MapsEventTypeAndFields(t *testing.T) {
	docID := uuid.New()
	wsID := uuid.New()
	q := &fakeQueue{id: "stream-1"}
	pub := NewQueuePublisher(q)

	err := pub.PublishDocumentEvent(context.Background(), service.DocumentEvent{
		EventID:     "evt-123",
		Type:        service.EventCreate,
		DocumentID:  docID,
		WorkspaceID: wsID,
		VersionNo:   3,
		Timestamp:   time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
	})
	assert.NoError(t, err)

	// The canonical domain.DocEvent the rag-worker decodes must carry the
	// domain event-type vocabulary ("document.create", not "create") and the
	// stringified ids — otherwise the pipeline switch and Qdrant payload break.
	assert.Equal(t, "evt-123", q.got.EventID)
	assert.Equal(t, domain.EventDocumentCreate, q.got.EventType)
	assert.Equal(t, docID.String(), q.got.DocumentID)
	assert.Equal(t, wsID.String(), q.got.WorkspaceID)
	assert.Equal(t, 3, q.got.VersionNo)
	assert.Equal(t, "2026-07-30T12:00:00Z", q.got.Timestamp)
}

func TestQueuePublisher_MapsAllEventTypes(t *testing.T) {
	cases := []struct {
		in   service.DocumentEventType
		want domain.EventType
	}{
		{service.EventCreate, domain.EventDocumentCreate},
		{service.EventUpdate, domain.EventDocumentUpdate},
		{service.EventDelete, domain.EventDocumentDelete},
		{service.EventPermissionChange, domain.EventPermissionChange},
	}
	for _, c := range cases {
		q := &fakeQueue{}
		pub := NewQueuePublisher(q)
		err := pub.PublishDocumentEvent(context.Background(), service.DocumentEvent{
			Type: c.in, DocumentID: uuid.New(), WorkspaceID: uuid.New(),
		})
		assert.NoError(t, err)
		assert.Equal(t, c.want, q.got.EventType, "mapping for %s", c.in)
	}
}

func TestQueuePublisher_AssignsEventIDWhenMissing(t *testing.T) {
	q := &fakeQueue{}
	pub := NewQueuePublisher(q)
	err := pub.PublishDocumentEvent(context.Background(), service.DocumentEvent{
		Type: service.EventUpdate, DocumentID: uuid.New(), WorkspaceID: uuid.New(),
	})
	assert.NoError(t, err)
	assert.NotEmpty(t, q.got.EventID, "missing event_id should be auto-generated")
}
