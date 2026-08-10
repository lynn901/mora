package authz

// delegated.go implements DelegatedManager (design-docs/13 §4.3, D5).
//
// A delegated session lets an internal service (the MCP Server) act on behalf
// of a principal (a user, or an agent acting for a user) for ≤30s, WITHOUT
// trusting a client-set header. The client holds ONLY a short-lived HS256 JWT
// carrying the session id (JTI); the authoritative, revocable record lives
// server-side in delegated_sessions (§5.1/§5.6).
//
// Why two parts: a signed JWT proves the bearer once held a valid session, but
// only the server-side row can say whether it was revoked or whether the
// workspace authz revision has moved (e.g. a permission was just yanked).
// VerifyDelegated checks BOTH: signature+expiry first, then the row. This is
// the linearization seam for "撤权后下一次请求同步拒绝" (§5.6): a revoke bumps
// workspace_authz_revisions.revision in the same tx, so the next Verify reads
// a stale issued_authz_revision and refuses.
//
// Signing reuses auth.TokenManager's HS256 (same JWT_SECRET) — no asymmetric
// key distribution ops (D5 rationale, §4.3).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// DelegatedTTL is the maximum lifetime of a delegated session (§5.6: ≤30s).
// Kept conservative; callers may shorten per-issuance.
const DelegatedTTL = 30 * time.Second

// DelegatedClaims is the JWT payload for a short-lived delegated context
// (12 §5.1). The client holds only this token; the server-side
// delegated_sessions row is authoritative on revocation + revision.
type DelegatedClaims struct {
	SessionID     string   `json:"sid"`                // delegated_sessions.id (JTI source)
	AgentID       string   `json:"aid,omitempty"`      // present when principal is an agent
	ActingUserID  string   `json:"uid,omitempty"`       // present when an agent acts for a user
	WorkspaceID   string   `json:"wsid"`
	Actions       []string `json:"act"`                 // allowed_actions snapshot
	AuthzRevision int64    `json:"rev"`                 // issued_authz_revision
	Audience      string   `json:"aud"`                 // target Provider/internal service
	jwt.RegisteredClaims
}

// DelegatedRequest is the input to IssueDelegated (§4.3 API contract).
type DelegatedRequest struct {
	AgentID       *uuid.UUID // required when principal is an agent
	ActingUserID  *uuid.UUID // present for agent-on-behalf-of-user
	WorkspaceID   uuid.UUID
	Actions       []domain.Action
	AuthzRevision int64  // issued_authz_revision (from Service.IssueDecision)
	Audience      string // target service, e.g. "mcp-server"
	TokenID       *uuid.UUID // api_tokens.id (FK target), when issued under an api token
}

// DelegatedSession is the authoritative server-side row projection.
type DelegatedSession struct {
	ID                uuid.UUID
	TokenID           *uuid.UUID
	AgentID           *uuid.UUID
	ActingUserID      *uuid.UUID
	WorkspaceID       uuid.UUID
	AllowedActions    []domain.Action
	IssuedAuthzRevision int64
	ExpiresAt         time.Time
	RevokedAt         *time.Time
}

// SessionRepo persists delegated_sessions (§5.1). The row is the authority:
// VerifyDelegated reads it after signature check; Revoke updates it + bumps the
// workspace authz revision in the same transaction (§5.6).
type SessionRepo interface {
	// Insert writes a new delegated_sessions row. issued_authz_revision and
	// expires_at come from the DelegatedManager (≤30s, §5.6).
	Insert(ctx context.Context, s DelegatedSession) error
	// Get loads a session by id (for VerifyDelegated).
	Get(ctx context.Context, id uuid.UUID) (DelegatedSession, error)
	// Revoke sets revoked_at=now AND bumps the workspace authz revision in the
	// same transaction (§5.6 linearization). Returns the new revision.
	Revoke(ctx context.Context, id, workspaceID uuid.UUID) (int64, error)
}

// DelegatedManager issues and verifies short-lived delegated sessions (§4.3).
// It signs HS256 with the same secret as auth.TokenManager (D5: reuse JWT base,
// no asymmetric key ops).
type DelegatedManager struct {
	secret  []byte
	ttl     time.Duration
	sessions SessionRepo
	revisions RevisionRepo
}

// NewDelegatedManager wires a DelegatedManager. secret MUST be the same
// JWT_SECRET auth.TokenManager uses (§4.3). ttl is clamped to DelegatedTTL.
func NewDelegatedManager(secret string, ttl time.Duration, sessions SessionRepo, revisions RevisionRepo) *DelegatedManager {
	if ttl <= 0 || ttl > DelegatedTTL {
		ttl = DelegatedTTL
	}
	return &DelegatedManager{secret: []byte(secret), ttl: ttl, sessions: sessions, revisions: revisions}
}

// ErrDelegatedExpired is returned when the JWT is past its expiry OR the
// server-side session has expired.
var ErrDelegatedExpired = errors.New("authz: delegated session expired")

// ErrDelegatedRevoked is returned when the server-side session row is revoked.
var ErrDelegatedRevoked = errors.New("authz: delegated session revoked")

// ErrDelegatedStaleRevision is returned when the workspace authz revision has
// advanced past the session's issued_authz_revision — the capability may have
// been narrowed/revoked since issuance (§5.6).
var ErrDelegatedStaleRevision = errors.New("authz: delegated session revision is stale")

// IssueDelegated signs a delegated JWT AND inserts the server-side
// delegated_sessions row. The two together are the capability: the JWT is the
// bearer proof, the row is the revocation authority. Expiry ≤30s (§5.6).
//
// The session insert is NOT in a transaction with any aggregate write here —
// the caller (the internal /internal/v1/authz/delegated endpoint) records the
// authorization_decision first (Service.IssueDecision), then issues this
// session. Revocation linearization is handled at Revoke time (same tx as
// revision+1).
func (m *DelegatedManager) IssueDelegated(ctx context.Context, req DelegatedRequest) (string, time.Time, error) {
	if req.WorkspaceID == uuid.Nil {
		return "", time.Time{}, errors.New("authz: delegated issue requires workspace_id")
	}
	if req.Audience == "" {
		return "", time.Time{}, errors.New("authz: delegated issue requires audience")
	}
	now := time.Now().UTC()
	expires := now.Add(m.ttl)
	sessionID := uuid.New()

	actions := req.Actions
	if actions == nil {
		actions = []domain.Action{}
	}
	claims := DelegatedClaims{
		SessionID:     sessionID.String(),
		WorkspaceID:   req.WorkspaceID.String(),
		Actions:       actionsToStrings(actions),
		AuthzRevision: req.AuthzRevision,
		Audience:      req.Audience,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        sessionID.String(), // JTI = session id
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
			Subject:   req.WorkspaceID.String(),
		},
	}
	if req.AgentID != nil {
		claims.AgentID = req.AgentID.String()
	}
	if req.ActingUserID != nil {
		claims.ActingUserID = req.ActingUserID.String()
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authz: sign delegated: %w", err)
	}

	// Insert the authoritative server-side row.
	if err := m.sessions.Insert(ctx, DelegatedSession{
		ID:                  sessionID,
		TokenID:             req.TokenID,
		AgentID:             req.AgentID,
		ActingUserID:        req.ActingUserID,
		WorkspaceID:         req.WorkspaceID,
		AllowedActions:      actions,
		IssuedAuthzRevision: req.AuthzRevision,
		ExpiresAt:           expires,
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("authz: insert delegated session: %w", err)
	}
	return signed, expires, nil
}

// VerifyDelegated validates signature+expiry, THEN loads delegated_sessions by
// session id to confirm: not revoked, not expired, authz_revision still current.
// It does NOT trust the JWT's own claims alone — the server-side row is
// authoritative (§5.1). A stale issued_authz_revision (workspace authz moved)
// → ErrDelegatedStaleRevision so the caller re-issues (§5.6).
func (m *DelegatedManager) VerifyDelegated(ctx context.Context, token string) (*DelegatedClaims, error) {
	var claims DelegatedClaims
	tok, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return m.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("authz: delegated signature: %w", err)
	}
	if !tok.Valid {
		return nil, errors.New("authz: delegated token invalid")
	}

	sessionID, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return nil, errors.New("authz: delegated session id malformed")
	}
	row, err := m.sessions.Get(ctx, sessionID)
	if err != nil {
		// No row = not a real session (forged JTI, or already purged).
		return nil, fmt.Errorf("authz: delegated session not found: %w", err)
	}
	if row.RevokedAt != nil {
		return nil, ErrDelegatedRevoked
	}
	if time.Now().UTC().After(row.ExpiresAt) {
		return nil, ErrDelegatedExpired
	}
	// §5.6 linearization: if the workspace authz revision advanced past the
	// issued one, the capability may have been narrowed — refuse, force re-issue.
	current, err := m.revisions.Current(ctx, row.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("authz: read authz revision: %w", err)
	}
	if current != row.IssuedAuthzRevision {
		return nil, ErrDelegatedStaleRevision
	}
	return &claims, nil
}

// Revoke revokes a delegated session AND bumps the workspace authz revision in
// the same transaction (§5.6). After this, VerifyDelegated returns
// ErrDelegatedRevoked (row) for any still-circulating JWT — and the bumped
// revision invalidates every other session issued under the prior revision
// (ErrDelegatedStaleRevision), giving the "撤权后下一次请求同步拒绝" guarantee.
func (m *DelegatedManager) Revoke(ctx context.Context, sessionID, workspaceID uuid.UUID) (int64, error) {
	return m.sessions.Revoke(ctx, sessionID, workspaceID)
}

// actionsToStrings is a helper for the JWT claim (actions are domain.Action,
// which is a string alias; the round-trip is lossless for Phase 0 vocab).
func actionsToStrings(actions []domain.Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, string(a))
	}
	return out
}
