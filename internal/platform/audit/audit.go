package audit

// Package audit provides the audit middleware that records key operations to
// the append-only audit log (07-security §5). It wraps HTTP handlers and
// captures actor/action/target for write operations.

import (
	"context"
	"time"

	"github.com/wiki/wiki-backend/internal/domain"
)

// Repo is the minimal audit append interface.
type Repo interface {
	Append(ctx context.Context, log *domain.AuditLog) error
}

// Logger wraps a Repo to provide a typed Record helper.
type Logger struct {
	repo Repo
}

func NewLogger(repo Repo) *Logger { return &Logger{repo: repo} }

// Record appends an audit entry. It never blocks the caller on failure: audit
// is best-effort relative to the request path (failures are logged elsewhere).
func (l *Logger) Record(ctx context.Context, actorType string, actorID *domain.UUID, action string,
	targetType string, targetID *domain.UUID, detail any, ip, ua string) {
	log := &domain.AuditLog{
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     detail,
		IPAddress:  ip,
		UserAgent:  ua,
		CreatedAt:  time.Now().UTC(),
	}
	_ = l.repo.Append(ctx, log)
}
