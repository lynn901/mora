// Package rbac holds the shared identity and permission types used across the
// modular monolith. The RBAC decision engine itself lives here in production
// (backed by the permissions tables — see design doc 03 §2.5); the MCP module
// reuses the same AuthContext and Permission vocabulary so identity flows
// consistently from Token → AuthContext → Mora API RBAC engine.
//
// Per design doc 06 §6.3, the MCP Server does NOT re-implement permission
// logic: it delegates RBAC to the Mora API (which consults this engine) and
// only enforces token-scope gating and existence-leak prevention locally.
package rbac

// Permission is a coarse capability on a target resource.
type Permission string

const (
	// PermRead allows viewing a resource.
	PermRead Permission = "read"
	// PermWrite allows creating / editing a resource.
	PermWrite Permission = "write"
	// PermAdmin allows managing a resource and its permissions.
	PermAdmin Permission = "admin"
)

// Scope is the capability envelope bound to an API token. It gates which
// MCP tools a token may invoke before any RBAC check (design doc 06 §6.3).
type Scope string

const (
	// ScopeReadOnly restricts the token to read-only tools.
	ScopeReadOnly Scope = "readonly"
	// ScopeReadWrite allows read and write tools (draft/review state).
	ScopeReadWrite Scope = "readwrite"
	// ScopeAdmin allows all tools including administrative ones.
	ScopeAdmin Scope = "admin"
)

// AllowsWrite reports whether the scope permits write operations.
func (s Scope) AllowsWrite() bool {
	return s == ScopeReadWrite || s == ScopeAdmin
}

// IdentityType is the kind of principal an API token is bound to.
type IdentityType string

const (
	// IdentityUser binds a token to a human user.
	IdentityUser IdentityType = "user"
	// IdentityServiceAccount binds a token to a service account.
	IdentityServiceAccount IdentityType = "service_account"
)
