package event

// Package event implements document event publishing to the message queue
// (Valkey Streams, stream: doc_events) per architecture §4.1. The RAG worker
// consumes these events; Mora never calls RAG directly (单向依赖).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/module/rag"
	"github.com/redis/go-redis/v9"
)

const streamKey = "doc_events"

// RedisPublisher publishes document events to a Redis/Valkey Stream.
type RedisPublisher struct {
	client *redis.Client
}

func NewRedisPublisher(client *redis.Client) *RedisPublisher {
	return &RedisPublisher{client: client}
}

func (p *RedisPublisher) PublishDocumentEvent(ctx context.Context, evt service.DocumentEvent) error {
	if evt.EventID == "" {
		evt.EventID = uuid.NewString()
	}
	evt.Timestamp = time.Now().UTC()
	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	return p.client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]any{
			"event_id":    evt.EventID,
			"event_type":  string(evt.Type),
			"document_id": evt.DocumentID.String(),
			"payload":     string(payload),
		},
	}).Err()
}

func (p *RedisPublisher) PublishModelRebuild(ctx context.Context, workspaceID string) error {
	_, err := p.client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]any{
			"event_id":   "model-rebuild-" + uuid.NewString(),
			"event_type": string(domain.EventModelRebuild),
			"payload":    "{}",
		},
	}).Result()
	return err
}

// NoopPublisher is a fallback publisher used when no MQ is configured (e.g.
// unit tests, single-node without RAG). It records events in-memory.
type NoopPublisher struct {
	Events []service.DocumentEvent
}

func NewNoopPublisher() *NoopPublisher { return &NoopPublisher{} }

func (p *NoopPublisher) PublishDocumentEvent(ctx context.Context, evt service.DocumentEvent) error {
	if evt.EventID == "" {
		evt.EventID = uuid.NewString()
	}
	evt.Timestamp = time.Now().UTC()
	p.Events = append(p.Events, evt)
	return nil
}

func (p *NoopPublisher) PublishModelRebuild(ctx context.Context, workspaceID string) error {
	return nil
}

// QueuePublisher adapts a rag.EventQueue (Valkey Streams producer) to the
// service.EventPublisher interface. It is the production publisher used by
// mora-api: document lifecycle events are mapped from service.DocumentEvent
// to the canonical domain.DocEvent and published through the same ValkeyQueue
// the rag-worker consumes, so producer and consumer share one stream format
// (field "event" = JSON(domain.DocEvent)) and one event-type vocabulary
// ("document.create" / "document.update" / ...). Without this adapter the
// worker's fieldsToEvent decoder would reject every event as malformed.
type QueuePublisher struct {
	Queue rag.EventQueue
}

// NewQueuePublisher wraps a rag.EventQueue (e.g. *mq.ValkeyQueue) as an
// EventPublisher. Pass a non-nil queue to switch mora-api from Noop to real
// Valkey Streams publishing.
func NewQueuePublisher(q rag.EventQueue) *QueuePublisher {
	return &QueuePublisher{Queue: q}
}

func (p *QueuePublisher) PublishDocumentEvent(ctx context.Context, evt service.DocumentEvent) error {
	if evt.EventID == "" {
		evt.EventID = uuid.NewString()
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	_, err := p.Queue.Publish(ctx, domain.DocEvent{
		EventID:       evt.EventID,
		EventType:     mapEventType(evt.Type),
		DocumentID:    evt.DocumentID.String(),
		WorkspaceID:   evt.WorkspaceID.String(),
		VersionNo:     evt.VersionNo,
		PrevVersionNo: evt.PrevVersionNo,
		Timestamp:     evt.Timestamp.UTC().Format(time.RFC3339),
	})
	return err
}

// PublishModelRebuild emits a model.rebuild event onto the canonical doc_events
// stream. The rag-worker consumes it (domain.EventModelRebuild) and re-indexes
// all published documents for the active model (05 §5.3).
func (p *QueuePublisher) PublishModelRebuild(ctx context.Context, workspaceID string) error {
	_, err := p.Queue.Publish(ctx, domain.DocEvent{
		EventID:     "model-rebuild-" + uuid.NewString(),
		EventType:   domain.EventModelRebuild,
		WorkspaceID: workspaceID,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	})
	return err
}

// mapEventType translates the mora-service event vocabulary into the canonical
// domain.EventType the RAG pipeline switches on (05-rag-pipeline-design §2.1).
func mapEventType(t service.DocumentEventType) domain.EventType {
	switch t {
	case service.EventCreate:
		return domain.EventDocumentCreate
	case service.EventUpdate:
		return domain.EventDocumentUpdate
	case service.EventDelete:
		return domain.EventDocumentDelete
	case service.EventPermissionChange:
		return domain.EventPermissionChange
	default:
		return domain.EventType(t)
	}
}
