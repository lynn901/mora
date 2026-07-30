package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
)

// AuditRepo appends audit log records. Per security design (07-security §5.2)
// audit_logs is append-only; this repo only ever INSERTs.
type AuditRepo struct{ db *DB }

func NewAuditRepo(db *DB) *AuditRepo { return &AuditRepo{db: db} }

func (r *AuditRepo) Append(ctx context.Context, log *domain.AuditLog) error {
	if log.ID == uuid.Nil {
		log.ID = uuid.New()
	}
	log.CreatedAt = time.Now().UTC()
	detail, _ := json.Marshal(log.Detail)
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO audit_logs (id, actor_type, actor_id, action, target_type, target_id, detail, ip_address, user_agent, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		log.ID, log.ActorType, log.ActorID, log.Action, log.TargetType, log.TargetID,
		detail, log.IPAddress, log.UserAgent, log.CreatedAt)
	return err
}
