package outbox

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// ValkeyPublisher is a StreamPublisher backed by a Valkey (Redis-compatible)
// client. It XADDs the event payload to the named stream verbatim — the outbox
// Dispatcher owns serialization, so this layer is byte-for-byte (§6.3).
//
// Streams are created lazily by XADD. Consumer groups are the consumer's
// concern (rag-worker creates knowledge_events groups on read); Phase 0 only
// guarantees events are not lost (they sit in the stream until a consumer
// attaches). MaxLen caps the stream so an unwired consumer cannot exhaust
// memory (best-effort; see §5.3 resilience).
type ValkeyPublisher struct {
	Rdb    *redis.Client
	MaxLen int64 // approximate cap per stream; 0 = uncapped
}

// NewValkeyPublisher builds a StreamPublisher over a Valkey client. maxLen caps
// each stream (approximate trim); pass 0 to leave streams uncapped.
func NewValkeyPublisher(rdb *redis.Client, maxLen int64) *ValkeyPublisher {
	if maxLen < 0 {
		maxLen = 0
	}
	return &ValkeyPublisher{Rdb: rdb, MaxLen: maxLen}
}

// Publish XADDs payload to stream and returns the Valkey message id.
func (p *ValkeyPublisher) Publish(ctx context.Context, stream string, payload []byte) (string, error) {
	if p.Rdb == nil {
		return "", fmt.Errorf("outbox: valkey publisher has no client")
	}
	args := &redis.XAddArgs{
		Stream: stream,
		MaxLen: p.MaxLen,
		Approx: p.MaxLen > 0,
		Values: map[string]interface{}{"payload": payload},
	}
	id, err := p.Rdb.XAdd(ctx, args).Result()
	if err != nil {
		return "", fmt.Errorf("outbox: xadd %s: %w", stream, err)
	}
	return id, nil
}

// Compile-time check.
var _ StreamPublisher = (*ValkeyPublisher)(nil)
