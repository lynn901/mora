// Package mq implements rag.EventQueue with Valkey (Redis-compatible) Streams.
// Stream: doc_events; consumer group: rag_pipeline_group; dead-letter: doc_events:dead
// (05-rag-pipeline-design.md §2). Reliability: ACK only after success, XAUTOCLAIM
// for crash recovery, idempotency handled by the worker.
package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/rag"
)

const (
	StreamName  = "doc_events"
	DeadStream  = "doc_events:dead"
	GroupName   = "rag_pipeline_group"
)

// ValkeyQueue is a Valkey/Redis Streams implementation of rag.EventQueue.
type ValkeyQueue struct {
	Rdb    *redis.Client
	Stream string
	Group  string
	Dead   string
}

func New(rdb *redis.Client) *ValkeyQueue {
	return &ValkeyQueue{Rdb: rdb, Stream: StreamName, Group: GroupName, Dead: DeadStream}
}

func (q *ValkeyQueue) Publish(ctx context.Context, ev domain.DocEvent) (string, error) {
	fields, err := eventToFields(ev)
	if err != nil {
		return "", err
	}
	id, err := q.Rdb.XAdd(ctx, &redis.XAddArgs{Stream: q.Stream, MaxLen: 100000, Approx: true, Values: fields}).Result()
	if err != nil {
		return "", err
	}
	// ensure the consumer group exists (no-op if present).
	_ = q.Rdb.XGroupCreateMkStream(ctx, q.Stream, q.Group, "$").Err()
	return id, nil
}

func (q *ValkeyQueue) ReadGroup(ctx context.Context, consumer string, count int64, block time.Duration) ([]rag.QueueMessage, error) {
	// ensure group exists on first read.
	if err := q.Rdb.XGroupCreateMkStream(ctx, q.Stream, q.Group, "$").Err(); err != nil && !isBusyGroup(err) {
		// best-effort; XREADGROUP will fail if group truly absent.
	}
	res, err := q.Rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: q.Group, Consumer: consumer, Streams: []string{q.Stream, ">"},
		Count: count, Block: block, NoAck: false,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var out []rag.QueueMessage
	for _, m := range res {
		for _, msg := range m.Messages {
			ev, ferr := fieldsToEvent(msg.Values)
			if ferr != nil {
				// malformed message: dead-letter it directly.
				_ = q.MoveToDeadLetter(ctx, rag.QueueMessage{Stream: q.Stream, ID: msg.ID}, "malformed: "+ferr.Error())
				_ = q.Ack(ctx, rag.QueueMessage{ID: msg.ID})
				continue
			}
			out = append(out, rag.QueueMessage{Stream: q.Stream, ID: msg.ID, DocEvent: ev, RawFields: toStringMap(msg.Values)})
		}
	}
	return out, nil
}

func (q *ValkeyQueue) Ack(ctx context.Context, msg rag.QueueMessage) error {
	return q.Rdb.XAck(ctx, q.Stream, q.Group, msg.ID).Err()
}

func (q *ValkeyQueue) MoveToDeadLetter(ctx context.Context, msg rag.QueueMessage, reason string) error {
	fields, _ := eventToFields(msg.DocEvent)
	fields["_dead_reason"] = reason
	fields["_origin_id"] = msg.ID
	fields["_dead_at"] = time.Now().UTC().Format(time.RFC3339)
	return q.Rdb.XAdd(ctx, &redis.XAddArgs{Stream: q.Dead, MaxLen: 10000, Approx: true, Values: fields}).Err()
}

func (q *ValkeyQueue) Claim(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]rag.QueueMessage, error) {
	msgs, _, err := q.Rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: q.Stream, Group: q.Group, Consumer: consumer,
		MinIdle: minIdle, Count: count, Start: "0-0",
	}).Result()
	if err != nil {
		return nil, err
	}
	var out []rag.QueueMessage
	for _, m := range msgs {
		ev, ferr := fieldsToEvent(m.Values)
		if ferr != nil {
			continue
		}
		out = append(out, rag.QueueMessage{Stream: q.Stream, ID: m.ID, DocEvent: ev, RawFields: toStringMap(m.Values)})
	}
	return out, nil
}

// toStringMap narrows map[string]any to map[string]string for the debug field.
func toStringMap(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// eventToFields serializes a DocEvent into Stream fields (flat strings).
func eventToFields(ev domain.DocEvent) (map[string]any, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	return map[string]any{"event": string(b)}, nil
}

func fieldsToEvent(vals map[string]any) (domain.DocEvent, error) {
	var ev domain.DocEvent
	s, ok := vals["event"].(string)
	if !ok {
		return ev, fmt.Errorf("missing event field")
	}
	if err := json.Unmarshal([]byte(s), &ev); err != nil {
		return ev, err
	}
	return ev, nil
}

func isBusyGroup(err error) bool {
	return err != nil && (err.Error() == "BUSYGROUP Consumer Group name already exists")
}
