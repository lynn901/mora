package domain

import (
	"time"

	"github.com/google/uuid"
)

// KnowledgeEvent is the unified event envelope (design-docs/12 §6.1, 13 §5.1).
// It carries only IDs, revisions, actions and necessary params — never content,
// credentials, full sessions or Skill packages. It coexists with DocEvent
// (which drives the existing doc_events RAG stream) and does not pollute it.
type KnowledgeEvent struct {
	EventID       string         `json:"event_id"`        // global idempotency key
	EventType     string         `json:"event_type"`      // e.g. "asset.version.requested"
	EventVersion  int            `json:"event_version"`
	AggregateType string         `json:"aggregate_type"`  // "knowledge_asset" | "agent" | ...
	AggregateID   uuid.UUID      `json:"aggregate_id"`
	WorkspaceID   *uuid.UUID     `json:"workspace_id,omitempty"`
	Actor         EventActor     `json:"actor"`           // {type, id}
	CorrelationID *uuid.UUID     `json:"correlation_id,omitempty"`
	CausationID   *uuid.UUID     `json:"causation_id,omitempty"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Payload       map[string]any `json:"payload,omitempty"`
}

// EventActor is the {type,id} pair identifying who triggered an event.
type EventActor struct {
	Type SubjectType `json:"type"`
	ID   uuid.UUID   `json:"id"`
}

// KnowledgeEventTypes enumerates the Phase 0 event types (13 §5.1).
const (
	KEAssetCreated           = "asset.created"
	KEAssetVersionRequested  = "asset.version.requested"
	KEAssetVersionActivated  = "asset.version.activated"
	KEAssetDeprecated        = "asset.deprecated"
	KEPermissionChanged      = "permission.changed"
	KEGovernanceDecision     = "governance.decision"
	KEAgentCreated           = "agent.created"
	KEAgentSuspended         = "agent.suspended"
	KEAgentBindingChanged    = "agent.binding_changed"
	KEAgentUseDenied         = "agent.use_denied"
	KEAuthzRevisionChanged   = "authz.revision_changed"
)

// KnowledgeAggregateTypes enumerates the Phase 0 aggregate types.
const (
	AggKnowledgeAsset = "knowledge_asset"
	AggAgent          = "agent"
	AggAuthz          = "workspace_authz"
)
