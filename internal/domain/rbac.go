package domain

import "time"

// RBAC domain types (对应 03-data-model.md §2.5)

type RoleScope string

const (
	ScopeSystem    RoleScope = "system"
	ScopeWorkspace RoleScope = "workspace"
	ScopeDirectory RoleScope = "directory"
	ScopePage      RoleScope = "page"
)

type Action string

const (
	ActionRead  Action = "read"
	ActionWrite Action = "write"
	ActionAdmin Action = "admin"
)

type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

type InheritScope string

const (
	InheritNodeOnly InheritScope = "node_only"
	InheritSubtree  InheritScope = "subtree"
)

type TargetType string

const (
	TargetWorkspace TargetType = "workspace"
	TargetDirectory TargetType = "directory"
	TargetDocument  TargetType = "document"
)

type SubjectType string

const (
	SubjectUser  SubjectType = "user"
	SubjectGroup SubjectType = "group"
)

type Role struct {
	ID          UUID      `json:"id"`
	Name        string    `json:"name"`
	Scope       RoleScope `json:"scope"`
	WorkspaceID *UUID     `json:"workspace_id,omitempty"`
	Permissions []Action  `json:"permissions"`
	IsSystem    bool      `json:"is_system"`
	CreatedAt   time.Time `json:"created_at"`
}

// Permission grants a subject (user/group) a role over a target resource.
type Permission struct {
	ID           UUID         `json:"id"`
	SubjectType  SubjectType  `json:"subject_type"`
	SubjectID    UUID         `json:"subject_id"`
	RoleID       UUID         `json:"role_id"`
	TargetType   TargetType   `json:"target_type"`
	TargetID     UUID         `json:"target_id"`
	Effect       Effect       `json:"effect"`
	InheritScope InheritScope `json:"inherit_scope"`
	CreatedAt    time.Time    `json:"created_at"`
	CreatedBy    *UUID        `json:"created_by,omitempty"`
}

// Grant is a resolved, in-memory permission entry used by the RBAC engine.
// It is the unit the engine evaluates: who (subject) gets what (actions) on
// which target, with what effect and inheritance.
type Grant struct {
	SubjectType  SubjectType
	SubjectID    UUID
	Actions      []Action
	TargetType   TargetType
	TargetID     UUID
	Effect       Effect
	InheritScope InheritScope
}

type AuditLog struct {
	ID         UUID      `json:"id"`
	ActorType  string    `json:"actor_type"`
	ActorID    *UUID     `json:"actor_id,omitempty"`
	Action     string    `json:"action"`
	TargetType string    `json:"target_type,omitempty"`
	TargetID   *UUID     `json:"target_id,omitempty"`
	Detail     any       `json:"detail"`
	IPAddress  string    `json:"ip_address,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type ApiToken struct {
	ID           UUID       `json:"id"`
	Name         string     `json:"name"`
	TokenHash    string     `json:"-"`
	Prefix       string     `json:"prefix"`
	IdentityType string     `json:"identity_type"`
	IdentityID   UUID       `json:"identity_id"`
	Scope        string     `json:"scope"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CreatedBy    *UUID      `json:"created_by,omitempty"`
}
