package asset

// authcontext.go defines the caller-identity context the asset read service uses
// for RBAC + audit. It mirrors source/service.AuthContext so the asset service
// stays self-contained (no import of the source package); the handler maps the
// HTTP AuthState to it.

import (
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// AuthContext carries the caller identity needed for RBAC + audit (mirrors
// source/service.AuthContext). IsAdmin short-circuits the Check (an admin
// bypasses per-resource RBAC, matching the document/source-service pattern).
type AuthContext struct {
	SubjectType     domain.SubjectType
	PrincipalID     uuid.UUID
	GroupIDs        []uuid.UUID
	IsAdmin         bool
	IsServiceCaller bool
}
