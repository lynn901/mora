// Package recall — audit port (design-docs/18 §9.4).
//
// AuditLogger is the local audit port the recall + feedback services use to
// record leak-safe allow/deny decisions (evidence.read, memory.feedback). It
// mirrors evidence.AuditLogger so the recall package is a leaf — no direct
// platform/audit dependency (which could cycle if platform/audit ever depends
// back on the memory module). The wiring layer (cmd/mora-api) injects a
// *audit.Logger, which satisfies this port (same Record signature).
package recall

import (
	"context"

	"github.com/google/uuid"
)

// AuditLogger is the audit port for recall + feedback (§9.4). It records
// allow/deny decisions for evidence.read + memory.feedback without blocking
// the decision (audit is best-effort relative to the request path, §5).
type AuditLogger interface {
	Record(ctx context.Context, actorType string, actorID *uuid.UUID, action string,
		targetType string, targetID *uuid.UUID, detail any, ip, ua string)
}
