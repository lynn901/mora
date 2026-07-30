package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// EventType enumerates document events that drive the RAG pipeline
// (05-rag-pipeline-design.md §2.1).
type EventType string

const (
	EventDocumentCreate     EventType = "document.create"
	EventDocumentUpdate     EventType = "document.update"
	EventDocumentDelete     EventType = "document.delete"
	EventAttachmentChange   EventType = "attachment.change"
	EventPermissionChange   EventType = "permission.change"
	EventModelRebuild       EventType = "model.rebuild"
)

// DocEvent is the message published to the Valkey Stream `doc_events`
// (05-rag-pipeline-design.md §2.2). EventID is the global idempotency key.
type DocEvent struct {
	EventID       string         `json:"event_id"`
	EventType     EventType      `json:"event_type"`
	DocumentID    string         `json:"document_id"`
	WorkspaceID   string         `json:"workspace_id"`
	DirectoryID   string         `json:"directory_id,omitempty"`
	VersionNo     int            `json:"version_no"`
	PrevVersionNo int            `json:"prev_version_no,omitempty"` // update: previous version (delete old chunks)
	Payload       map[string]any `json:"payload,omitempty"`
	Timestamp     string         `json:"timestamp"`
}

// subject IDs use the "user:<id>" / "group:<id>" form (05 §4.3).
func UserSubject(id string) string  { return "user:" + id }
func GroupSubject(id string) string { return "group:" + id }

// slug lowercases and replaces non [a-z0-9] runs with '_' for collection names.
func slug(s string) string {
	b := make([]byte, 0, len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, byte(r))
		default:
			b = append(b, '_')
		}
	}
	out := strings.Trim(string(b), "_")
	if out == "" {
		return "x"
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// PointID computes the deterministic Qdrant point id for a chunk:
// uuid5(namespace, document_id + version_no + chunk_index) (05 §3.5).
// Repeated upsert with the same id overwrites rather than duplicates.
func PointID(namespace, documentID string, versionNo, chunkIndex int) string {
	name := fmt.Sprintf("%s|%d|%d", documentID, versionNo, chunkIndex)
	return uuid5(namespace, name).String()
}
