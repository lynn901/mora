package event

// Package event implements document event publishing to the message queue
// (Valkey Streams, stream: doc_events) per architecture §4.1. The RAG worker
// consumes these events; Wiki never calls RAG directly (单向依赖).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/wiki/wiki-backend/internal/module/wiki/service"
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
