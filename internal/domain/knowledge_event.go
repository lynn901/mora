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
	AggMemoryEvidence = "memory_evidence" // Phase 4 (18 §7.4)
)

// Phase 4 memory event types (design-docs/18 §3.3, §7.4). These flow on the
// memory_events Stream + memory_distill consumer group; the knowledge-worker
// maps them to memory_extract / memory_dedup / memory_revalidate jobs.
const (
	KEEvidenceCaptured    = "evidence.captured"    // → memory_extract Job
	KEEvidenceExtracted   = "evidence.extract"     // → memory_dedup Job
	KEvidenceRevalidate   = "evidence.revalidate"  // → memory_revalidate Job
)

// MemoryEventsStream is the Valkey Stream memory events are published to
// (design-docs/18 §3.3, D5; the 12 §6.2 pre-reserved split). The outbox
// dispatcher publishes here; the knowledge-worker's memory_distill consumer
// group reads it. Phase 4 first version wires capture + extract.
const MemoryEventsStream = "memory_events"

// MemoryDistillGroup is the consumer group over memory_events (18 §3.3, D5).
const MemoryDistillGroup = "memory_distill"
